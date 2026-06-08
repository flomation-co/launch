package http

import (
	"fmt"
	"net/http"
	"time"

	"flomation.app/automate/launch/internal/agent"
	appmetrics "flomation.app/automate/launch/internal/metrics"
	"flomation.app/automate/launch/internal/twilio"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// handleTwilioSMSWebhook handles POST /webhook/twilio/sms/:id
// Receives incoming SMS messages from Twilio. :id is a trigger_id for
// new trigger-keyed registrations or an agent_id for legacy.
func (s *Service) handleTwilioSMSWebhook(c *gin.Context) {
	id := c.Param("id")
	if err := uuid.Validate(id); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, _ := s.trigger.GetTriggerByID(id)
	isTrigger := tr != nil && (tr.Type == "twilio-sms" || tr.Type == "twilio_sms")

	// Parse form-encoded webhook data
	if err := c.Request.ParseForm(); err != nil {
		log.WithError(err).Warn("failed to parse Twilio SMS webhook form")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	from := c.Request.FormValue("From")
	to := c.Request.FormValue("To")
	body := c.Request.FormValue("Body")
	messageSID := c.Request.FormValue("MessageSid")
	accountSID := c.Request.FormValue("AccountSid")

	if from == "" || body == "" {
		c.Data(http.StatusOK, "application/xml", []byte(twilio.EmptyResponse()))
		return
	}

	// Resolve auth token for signature validation.
	var authToken string
	if isTrigger {
		authToken = s.resolveTriggerCreds(id)["auth_token"]
	} else {
		reg, err := s.agent.GetRegistration(id)
		if err != nil || reg == nil {
			log.WithField("agent_id", id).Warn("SMS webhook for unknown agent")
			c.Data(http.StatusOK, "application/xml", []byte(twilio.EmptyResponse()))
			return
		}
		authToken = extractTwilioAuthToken(reg.Channels, accountSID)
	}
	if authToken != "" {
		signature := c.GetHeader("X-Twilio-Signature")
		params := make(map[string]string)
		for k, v := range c.Request.Form {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
		requestURL := fmt.Sprintf("%s%s", s.config.PublicURL, c.Request.URL.Path)
		if !twilio.ValidateSignature(authToken, requestURL, signature, params) {
			log.WithField("id", id).Warn("invalid Twilio signature on SMS webhook")
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
	}

	// Normalise phone numbers to E.164
	from = twilio.NormaliseE164(from)
	to = twilio.NormaliseE164(to)

	sender := from
	metadata := map[string]interface{}{
		// Canonical keys
		"channel_id": to,
		"user_id":    from,
		"user_name":  from,
		// Provider-specific
		"from":        from,
		"to":          to,
		"message_sid": messageSID,
		"account_sid": accountSID,
		"date":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	// Dispatch asynchronously — Twilio expects a quick response
	appmetrics.InboundMessagesTotal.WithLabelValues("twilio_sms").Inc()
	go func() {
		if isTrigger {
			s.postTriggerDispatch(id, "twilio_sms", sender, body, metadata)
			return
		}
		msg := agent.InboundMessage{
			ChannelType: "twilio_sms",
			Sender:      sender,
			Content:     body,
			Metadata:    metadata,
		}
		if err := s.agent.HandleInboundMessage(id, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": id,
				"error":    err,
				"from":     from,
			}).Error("failed to handle Twilio SMS message")
		}
	}()

	// Return empty TwiML — reply is sent via the messaging action, not here
	c.Data(http.StatusOK, "application/xml", []byte(twilio.EmptyResponse()))
}

// extractTwilioAuthToken finds the Twilio auth token from agent channel config.
func extractTwilioAuthToken(channels interface{}, accountSID string) string {
	channelList, ok := channels.([]interface{})
	if !ok {
		return ""
	}
	for _, ch := range channelList {
		chMap, ok := ch.(map[string]interface{})
		if !ok {
			continue
		}
		chType, _ := chMap["type"].(string)
		if chType != "twilio_sms" && chType != "twilio_voice" {
			continue
		}
		cfg, ok := chMap["config"].(map[string]interface{})
		if !ok {
			continue
		}
		if token, ok := cfg["auth_token"].(string); ok && token != "" {
			return token
		}
	}
	return ""
}
