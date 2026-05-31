package twilio

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// VoiceCall tracks an active voice call session.
type VoiceCall struct {
	SessionID    string
	StreamSID    string
	CallSID      string
	AgentID      string
	CallerNumber string
	TwilioNumber string
	StartedAt    time.Time
	Outbound     bool // true if pre-registered by make_call action

	// TwilioConn is the WebSocket connection from Twilio (media stream).
	TwilioConn *websocket.Conn

	// ExecutorConn is the WebSocket connection from the executor (via API proxy).
	ExecutorConn *websocket.Conn

	mu     sync.Mutex
	closed bool

	// bridgeReady is closed when the executor connects and bridging can begin.
	bridgeReady chan struct{}

	// twilioReady is closed when Twilio connects. Used for outbound calls
	// where the executor connects before Twilio (callee hasn't answered yet).
	twilioReady chan struct{}
}

// VoiceCallManager manages active voice call sessions.
type VoiceCallManager struct {
	mu       sync.RWMutex
	sessions map[string]*VoiceCall // session_id → VoiceCall
}

// NewVoiceCallManager creates a manager with a background cleanup goroutine.
func NewVoiceCallManager() *VoiceCallManager {
	m := &VoiceCallManager{
		sessions: make(map[string]*VoiceCall),
	}
	go m.cleanup()
	return m
}

// Create registers a new voice call session.
func (m *VoiceCallManager) Create(sessionID, agentID, callerNumber, twilioNumber string) *VoiceCall {
	m.mu.Lock()
	defer m.mu.Unlock()

	vc := &VoiceCall{
		SessionID:    sessionID,
		AgentID:      agentID,
		CallerNumber: callerNumber,
		TwilioNumber: twilioNumber,
		StartedAt:    time.Now(),
		bridgeReady:  make(chan struct{}),
		twilioReady:  make(chan struct{}),
	}
	m.sessions[sessionID] = vc

	log.WithFields(log.Fields{
		"session_id": sessionID,
		"agent_id":   agentID,
		"caller":     callerNumber,
	}).Info("voice call session created")

	return vc
}

// Get returns a voice call by session ID.
func (m *VoiceCallManager) Get(sessionID string) *VoiceCall {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[sessionID]
}

// Remove deletes a voice call session and closes its connections.
func (m *VoiceCallManager) Remove(sessionID string) {
	m.mu.Lock()
	vc, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if vc != nil {
		vc.Close()
	}
}

// Close terminates all connections for the voice call.
func (vc *VoiceCall) Close() {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	if vc.closed {
		return
	}
	vc.closed = true

	if vc.TwilioConn != nil {
		_ = vc.TwilioConn.Close()
	}
	if vc.ExecutorConn != nil {
		_ = vc.ExecutorConn.Close()
	}

	log.WithFields(log.Fields{
		"session_id": vc.SessionID,
		"duration_s": int(time.Since(vc.StartedAt).Seconds()),
	}).Info("voice call session closed")
}

// SetTwilioConn sets the Twilio-side WebSocket connection.
func (vc *VoiceCall) SetTwilioConn(conn *websocket.Conn) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.TwilioConn = conn

	// Signal that Twilio has connected (used for outbound calls)
	select {
	case <-vc.twilioReady:
		// Already closed
	default:
		close(vc.twilioReady)
	}
}

// WaitForTwilio blocks until Twilio connects or the timeout expires.
// Used for outbound calls where the executor connects before Twilio.
func (vc *VoiceCall) WaitForTwilio(timeout time.Duration) bool {
	select {
	case <-vc.twilioReady:
		return true
	case <-time.After(timeout):
		return false
	}
}

// SetExecutorConn sets the executor-side WebSocket connection and signals
// that bridging can begin.
func (vc *VoiceCall) SetExecutorConn(conn *websocket.Conn) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.ExecutorConn = conn

	// Signal that the executor has connected
	select {
	case <-vc.bridgeReady:
		// Already closed
	default:
		close(vc.bridgeReady)
	}
}

// WaitForExecutor blocks until the executor connects or the timeout expires.
func (vc *VoiceCall) WaitForExecutor(timeout time.Duration) bool {
	select {
	case <-vc.bridgeReady:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Bridge relays messages between Twilio and executor WebSockets.
// Runs until either connection closes. Should be called in a goroutine.
func (vc *VoiceCall) Bridge() {
	done := make(chan struct{})

	// Twilio → Executor
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			vc.mu.Lock()
			tw := vc.TwilioConn
			ex := vc.ExecutorConn
			vc.mu.Unlock()

			if tw == nil || ex == nil {
				return
			}

			msgType, data, err := tw.ReadMessage()
			if err != nil {
				return
			}
			if err := ex.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	}()

	// Executor → Twilio
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			vc.mu.Lock()
			tw := vc.TwilioConn
			ex := vc.ExecutorConn
			vc.mu.Unlock()

			if tw == nil || ex == nil {
				return
			}

			msgType, data, err := ex.ReadMessage()
			if err != nil {
				return
			}
			if err := tw.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	}()

	// Wait for either direction to complete
	<-done
	vc.Close()
}

// cleanup periodically removes stale sessions (older than 1 hour).
func (m *VoiceCallManager) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.Lock()
		for id, vc := range m.sessions {
			if time.Since(vc.StartedAt) > time.Hour {
				delete(m.sessions, id)
				go vc.Close()
				log.WithField("session_id", id).Warn("stale voice call session cleaned up")
			}
		}
		m.mu.Unlock()
	}
}