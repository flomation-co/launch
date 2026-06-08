package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch/internal/agent"
	appmetrics "flomation.app/automate/launch/internal/metrics"
	"flomation.app/automate/launch/internal/twilio"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleTwilioVoiceWebhook handles POST /webhook/twilio/voice/:id
// Initial voice call webhook from Twilio. Responds with TwiML that
// starts a bidirectional Media Stream.
//
// :id is currently always interpreted as an agent_id (voice is realtime
// + WebSocket-bridged, which requires the agent session loop). Trigger-
// only voice flows are out of scope for the trigger-keyed refactor and
// could be added later as a follow-up.
func (s *Service) handleTwilioVoiceWebhook(c *gin.Context) {
	agentID := c.Param("id")
	if err := uuid.Validate(agentID); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Parse form-encoded call data
	_ = c.Request.ParseForm()
	from := twilio.NormaliseE164(c.Request.FormValue("From"))
	to := twilio.NormaliseE164(c.Request.FormValue("To"))
	callSID := c.Request.FormValue("CallSid")

	log.WithFields(log.Fields{
		"agent_id": agentID,
		"from":     from,
		"to":       to,
		"call_sid": callSID,
	}).Info("incoming Twilio voice call")

	// Create voice session
	sessionID := uuid.New().String()
	vc := s.voiceCalls.Create(sessionID, agentID, from, to)
	vc.CallSID = callSID

	// Build WebSocket URL for Media Stream
	wsScheme := "wss"
	publicURL := s.config.PublicURL
	if strings.HasPrefix(publicURL, "http://") {
		wsScheme = "ws"
		publicURL = strings.TrimPrefix(publicURL, "http://")
	} else {
		publicURL = strings.TrimPrefix(publicURL, "https://")
	}
	wsURL := fmt.Sprintf("%s://%s/ws/twilio/voice/%s", wsScheme, publicURL, agentID)

	// Respond with TwiML to start the media stream
	params := map[string]string{
		"agentId":   agentID,
		"sessionId": sessionID,
		"from":      from,
		"to":        to,
	}
	twiml := twilio.MediaStreamResponse(wsURL, params)

	appmetrics.InboundMessagesTotal.WithLabelValues("twilio_voice").Inc()
	c.Data(http.StatusOK, "application/xml", []byte(twiml))
}

// handleTwilioVoiceWS handles GET /ws/twilio/voice/:agent_id
// This is the WebSocket endpoint that Twilio connects to for the media stream.
func (s *Service) handleTwilioVoiceWS(c *gin.Context) {
	agentID := c.Param("agent_id")

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.WithError(err).Error("failed to upgrade Twilio voice WebSocket")
		return
	}

	// Wait for the "start" event to get session metadata
	var sessionID, streamSID, callSID, from, to string
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	for {
		_, rawMsg, err := conn.ReadMessage()
		if err != nil {
			log.WithError(err).Warn("voice WS: error waiting for start event")
			_ = conn.Close()
			return
		}

		var msg struct {
			Event string `json:"event"`
			Start *struct {
				StreamSID        string            `json:"streamSid"`
				CallSID          string            `json:"callSid"`
				CustomParameters map[string]string `json:"customParameters"`
			} `json:"start,omitempty"`
		}
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}

		if msg.Event == "connected" {
			continue
		}
		if msg.Event == "start" && msg.Start != nil {
			streamSID = msg.Start.StreamSID
			callSID = msg.Start.CallSID
			sessionID = msg.Start.CustomParameters["sessionId"]
			from = msg.Start.CustomParameters["from"]
			to = msg.Start.CustomParameters["to"]
			break
		}
	}

	_ = conn.SetReadDeadline(time.Time{}) // Remove deadline

	if sessionID == "" {
		log.Warn("voice WS: no sessionId in start event")
		_ = conn.Close()
		return
	}

	isOutbound := false

	// Check if this session was pre-registered (outbound call) or created
	// by the inbound voice webhook.
	vc := s.voiceCalls.Get(sessionID)
	if vc != nil && vc.Outbound {
		// Session was pre-registered by make_call action
		isOutbound = true
		vc.StreamSID = streamSID
		vc.CallSID = callSID
		vc.SetTwilioConn(conn)

		log.WithFields(log.Fields{
			"session_id": sessionID,
			"stream_sid": streamSID,
			"call_sid":   callSID,
			"agent_id":   agentID,
			"outbound":   true,
		}).Info("Twilio media stream connected (outbound)")
	} else {
		// Inbound call -- session may already exist (created by webhook) or be new
		if vc == nil {
			vc = s.voiceCalls.Create(sessionID, agentID, from, to)
		}
		vc.StreamSID = streamSID
		vc.CallSID = callSID
		vc.SetTwilioConn(conn)

		log.WithFields(log.Fields{
			"session_id": sessionID,
			"stream_sid": streamSID,
			"call_sid":   callSID,
			"agent_id":   agentID,
		}).Info("Twilio media stream connected (inbound)")

		// Dispatch flow execution for inbound calls only
		go func() {
			metadata := map[string]interface{}{
				"channel_id": to,
				"user_id":    from,
				"user_name":  from,
				"from":       from,
				"to":         to,
				"call_sid":   callSID,
				"stream_sid": streamSID,
				"session_id": sessionID,
				"is_voice":   true,
			}

			msg := agent.InboundMessage{
				ChannelType: "twilio_voice",
				Sender:      from,
				Content:     "",
				Metadata:    metadata,
			}

			if err := s.agent.HandleInboundMessage(agentID, msg); err != nil {
				log.WithFields(log.Fields{
					"agent_id":   agentID,
					"session_id": sessionID,
					"error":      err,
				}).Error("failed to dispatch voice call flow")
			}
		}()
	}

	_ = isOutbound // used for logging above

	// Wait for the executor to connect, then bridge
	if !vc.WaitForExecutor(30 * time.Second) {
		log.WithField("session_id", sessionID).Warn("executor did not connect within timeout")
		s.voiceCalls.Remove(sessionID)
		return
	}

	// Bridge messages between Twilio and executor
	// This blocks until the call ends
	vc.Bridge()

	// Cleanup
	s.voiceCalls.Remove(sessionID)
}

// handleTwilioVoiceStatus handles POST /webhook/twilio/voice/:agent_id/status
// Called by Twilio when the call status changes (completed, busy, etc.)
func (s *Service) handleTwilioVoiceStatus(c *gin.Context) {
	_ = c.Request.ParseForm()
	callSID := c.Request.FormValue("CallSid")
	status := c.Request.FormValue("CallStatus")

	log.WithFields(log.Fields{
		"call_sid": callSID,
		"status":   status,
	}).Info("Twilio voice call status update")

	c.Status(http.StatusOK)
}

// handleVoiceSessionInternal handles GET /internal/voice-session/:session_id
// This is the internal WebSocket endpoint that the executor (via API proxy) connects to.
func (s *Service) handleVoiceSessionInternal(c *gin.Context) {
	sessionID := c.Param("session_id")

	vc := s.voiceCalls.Get(sessionID)
	if vc == nil {
		log.WithField("session_id", sessionID).Debug("voice session not found for internal WS (likely already ended)")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.WithError(err).Error("failed to upgrade internal voice session WebSocket")
		return
	}

	log.WithField("session_id", sessionID).Info("executor connected to voice session")

	// For outbound calls, Twilio may not have connected yet (callee hasn't
	// answered). Wait for the Twilio side before sending start events so
	// the executor receives valid StreamSID and CallSID.
	if vc.TwilioConn == nil {
		log.WithField("session_id", sessionID).Info("waiting for Twilio to connect (outbound call)")
		if !vc.WaitForTwilio(60 * time.Second) {
			log.WithField("session_id", sessionID).Warn("Twilio did not connect within timeout (outbound call not answered?)")
			_ = conn.Close()
			s.voiceCalls.Remove(sessionID)
			return
		}
		log.WithField("session_id", sessionID).Info("Twilio connected for outbound call")
	}

	// Forward the initial "connected" and "start" events that we already consumed
	// The executor's voice_session action expects these events.
	connectedMsg, _ := json.Marshal(map[string]string{
		"event":    "connected",
		"protocol": "Call",
		"version":  "1.0.0",
	})
	_ = conn.WriteMessage(websocket.TextMessage, connectedMsg)

	startMsg, _ := json.Marshal(map[string]interface{}{
		"event":     "start",
		"streamSid": vc.StreamSID,
		"start": map[string]interface{}{
			"streamSid": vc.StreamSID,
			"callSid":   vc.CallSID,
			"mediaFormat": map[string]interface{}{
				"encoding":   "audio/x-mulaw",
				"sampleRate": 8000,
				"channels":   1,
			},
			"customParameters": map[string]string{
				"agentId":   vc.AgentID,
				"sessionId": vc.SessionID,
			},
		},
	})
	_ = conn.WriteMessage(websocket.TextMessage, startMsg)

	// Link the executor WebSocket to the voice call session
	// This signals the WaitForExecutor() to unblock and start bridging
	vc.SetExecutorConn(conn)
}

// handleVoiceSessionRegister handles POST /internal/voice-session/:session_id/register
// Pre-registers a voice session for outbound calls. The make_call action calls
// this before placing the Twilio call so Launch knows not to dispatch a new flow
// when Twilio connects. Returns the WebSocket URL for the TwiML.
func (s *Service) handleVoiceSessionRegister(c *gin.Context) {
	sessionID := c.Param("session_id")

	var body struct {
		AgentID      string `json:"agent_id"`
		CallerNumber string `json:"caller_number"`
		TwilioNumber string `json:"twilio_number"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	vc := s.voiceCalls.Create(sessionID, body.AgentID, body.CallerNumber, body.TwilioNumber)
	vc.Outbound = true

	// Build the public WebSocket URL for the outbound Media Stream
	wsScheme := "wss"
	publicURL := s.config.PublicURL
	if strings.HasPrefix(publicURL, "http://") {
		wsScheme = "ws"
		publicURL = strings.TrimPrefix(publicURL, "http://")
	} else {
		publicURL = strings.TrimPrefix(publicURL, "https://")
	}
	wsURL := fmt.Sprintf("%s://%s/ws/twilio/voice-outbound/%s", wsScheme, publicURL, sessionID)

	log.WithFields(log.Fields{
		"session_id": sessionID,
		"agent_id":   body.AgentID,
	}).Info("voice session pre-registered for outbound call")

	c.JSON(http.StatusCreated, gin.H{
		"session_id": sessionID,
		"ws_url":     wsURL,
	})
}

// handleTwilioVoiceOutboundWS handles GET /ws/twilio/voice-outbound/:session_id
// WebSocket endpoint for outbound calls. Uses session_id (not agent_id) since
// the session is pre-registered and no flow dispatch is needed.
func (s *Service) handleTwilioVoiceOutboundWS(c *gin.Context) {
	sessionID := c.Param("session_id")

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.WithError(err).Error("failed to upgrade outbound voice WebSocket")
		return
	}

	// Wait for the "start" event from Twilio
	var streamSID, callSID string
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	for {
		_, rawMsg, err := conn.ReadMessage()
		if err != nil {
			log.WithError(err).Warn("outbound voice WS: error waiting for start event")
			_ = conn.Close()
			return
		}

		var msg struct {
			Event string `json:"event"`
			Start *struct {
				StreamSID        string            `json:"streamSid"`
				CallSID          string            `json:"callSid"`
				CustomParameters map[string]string `json:"customParameters"`
			} `json:"start,omitempty"`
		}
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}

		if msg.Event == "connected" {
			continue
		}
		if msg.Event == "start" && msg.Start != nil {
			streamSID = msg.Start.StreamSID
			callSID = msg.Start.CallSID
			break
		}
	}

	_ = conn.SetReadDeadline(time.Time{})

	// Look up the pre-registered session
	vc := s.voiceCalls.Get(sessionID)
	if vc == nil {
		log.WithField("session_id", sessionID).Warn("outbound voice WS: session not found (not pre-registered)")
		_ = conn.Close()
		return
	}

	vc.StreamSID = streamSID
	vc.CallSID = callSID
	vc.SetTwilioConn(conn)

	log.WithFields(log.Fields{
		"session_id": sessionID,
		"stream_sid": streamSID,
		"call_sid":   callSID,
	}).Info("outbound Twilio media stream connected")

	// Wait for the executor to connect, then bridge
	if !vc.WaitForExecutor(30 * time.Second) {
		log.WithField("session_id", sessionID).Warn("executor did not connect within timeout (outbound)")
		s.voiceCalls.Remove(sessionID)
		return
	}

	vc.Bridge()
	s.voiceCalls.Remove(sessionID)
}
