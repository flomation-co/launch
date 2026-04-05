// Package agent provides the agent orchestration service for the Launch process.
// It manages agent lifecycles, leases, heartbeats, and message dispatch,
// following the same patterns as the S3 trigger service for horizontal scaling.
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/telegram"
	"flomation.app/automate/launch/internal/trigger"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	leaseDuration     = 2 * time.Minute
	heartbeatInterval = 30 * time.Second
	watchdogInterval  = 60 * time.Second
	startupDelay      = 5 * time.Second
	apiTimeout        = 30 * time.Second
)

// Service manages agent runtimes within the Launch process.
// Each Launch instance generates a unique instanceID and uses lease-based
// ownership to coordinate with other instances.
type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	telegram   *telegram.Service
	instanceID string

	mu            sync.RWMutex
	managedAgents map[string]*managedAgent // agentID → active management state
}

// managedAgent tracks runtime state for an agent this instance is managing.
type managedAgent struct {
	reg      *launch.AgentRegistration
	stopCh   chan struct{}
	stopped  bool
}

// NewService creates and starts the agent orchestration service.
func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service, telegramSvc *telegram.Service) *Service {
	s := &Service{
		config:        config,
		db:            db,
		trigger:       trigger,
		telegram:      telegramSvc,
		instanceID:    uuid.New().String(),
		managedAgents: make(map[string]*managedAgent),
	}

	go s.watchdog()
	go s.heartbeatLoop()

	log.WithFields(log.Fields{
		"instance_id": s.instanceID,
	}).Info("agent service started")

	return s
}

// RegisterAgent is called (via HTTP) when the API starts an agent.
// It upserts the registration and begins managing the agent.
func (s *Service) RegisterAgent(reg launch.AgentRegistration) error {
	if err := s.db.UpsertAgentRegistration(reg); err != nil {
		return fmt.Errorf("failed to register agent: %w", err)
	}

	s.startManaging(&reg)
	s.activateChannels(&reg)

	log.WithFields(log.Fields{
		"agent_id":    reg.AgentID,
		"instance_id": s.instanceID,
	}).Info("agent registered and management started")

	return nil
}

// DeregisterAgent is called (via HTTP) when the API stops an agent.
func (s *Service) DeregisterAgent(agentID string) error {
	s.deactivateChannels(agentID)
	s.stopManaging(agentID)

	if err := s.db.DisableAgentRegistration(agentID); err != nil {
		return fmt.Errorf("failed to disable agent registration: %w", err)
	}

	_ = s.db.ReleaseAgentLease(agentID, s.instanceID)

	log.WithFields(log.Fields{
		"agent_id":    agentID,
		"instance_id": s.instanceID,
	}).Info("agent deregistered")

	return nil
}

// GetRegistration returns the agent registration for channel config access.
func (s *Service) GetRegistration(agentID string) (*launch.AgentRegistration, error) {
	return s.db.GetAgentRegistration(agentID)
}

// defaultHistoryLimit is the number of prior turns we pull into the
// conversation_history field of trigger data. The AI actions default to
// the same value when consuming it, so 20 turns of context is the
// Phase 1 working setpoint.
const defaultHistoryLimit = 20

// HandleInboundMessage processes an incoming message for an agent.
// This is called from HTTP webhook handlers and is stateless — any instance can handle it.
//
// Phase 1 of the Agent Memory feature added identity + conversation
// resolution: before the inbound turn is stored, we resolve the sender
// to a canonical AgentUser and pick (or open) the conversation scoped
// to (agent, user, channel, thread). Prior history is fetched from
// that conversation *before* the current turn is written, so the AI
// action sees only turns that precede the new one.
//
// If identity or conversation resolution fails (e.g. the API is
// briefly unreachable) we log and fall back to an unscoped dispatch —
// a degraded agent response is better than a dropped user message.
func (s *Service) HandleInboundMessage(agentID string, message InboundMessage) error {
	reg, err := s.db.GetAgentRegistration(agentID)
	if err != nil {
		return fmt.Errorf("failed to get agent registration: %w", err)
	}
	if reg == nil {
		return fmt.Errorf("agent %s is not registered", agentID)
	}

	// If the agent was disabled (e.g. after a failed execution or stale state),
	// auto-recover by re-enabling it. The webhook is still arriving, which means
	// the external channel is still active and the user expects the agent to work.
	if reg.DisabledAt != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
		}).Info("auto-recovering disabled agent registration")
		if err := s.db.UpsertAgentRegistration(*reg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Error("failed to re-enable agent registration")
		}
	}

	return s.handleInboundMessageForReg(reg, message)
}

// handleInboundMessageForReg is the post-registration-lookup orchestrator
// for an inbound message. It runs the Phase 1 Agent Memory pipeline:
// resolve identity → resolve conversation → fetch scoped history →
// store inbound turn → dispatch execution, with each step's output
// threaded into the next.
//
// This is separated from HandleInboundMessage so the end-to-end flow
// can be exercised by tests that stub the API via httptest without
// needing a real persistence.Service for the registration lookup.
func (s *Service) handleInboundMessageForReg(reg *launch.AgentRegistration, message InboundMessage) error {
	agentID := reg.AgentID

	// Step 1: resolve the sender to a canonical agent_user via the
	// stable channel-specific external id (e.g. Slack U-id, Telegram
	// numeric sender id). Auto-creates user+identity on first contact.
	var agentUserID *string
	if id, err := s.resolveIdentity(reg, message); err != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
			"error":    err,
		}).Warn("failed to resolve agent identity, continuing without user scoping")
	} else if id != nil {
		agentUserID = &id.User.ID
	}

	// Step 2: open/continue a conversation scoped to (agent, user,
	// channel, thread). Slack threads each get their own conversation;
	// a new DM (no thread_ts) reuses the same open conversation until
	// it is explicitly closed.
	var conversationID *string
	channelID, threadID := deriveChannelScope(message)
	if channelID != "" {
		if conv, err := s.resolveConversation(reg, agentUserID, message.ChannelType, channelID, threadID); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Warn("failed to resolve agent conversation, continuing without conversation scoping")
		} else if conv != nil {
			conversationID = &conv.ID
		}
	}

	// Step 3: fetch conversation history BEFORE storing the current
	// turn. If we stored first the AI action would see its own prompt
	// echoed back and loop on it (this was the bug that motivated the
	// Phase 1 work).
	var conversationHistory []map[string]interface{}
	if conversationID != nil {
		conversationHistory = s.fetchConversationHistory(reg, *conversationID, defaultHistoryLimit)
	}

	// Step 4: store the inbound turn. When we have a conversation_id
	// the message is written into that conversation with an
	// auto-assigned sequence number; otherwise we fall back to the
	// legacy agent-wide storage path.
	msgID, err := s.storeMessage(reg, message, conversationID)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
			"error":    err,
		}).Error("failed to store agent message")
		// Continue to dispatch even if message storage fails
	}

	// Step 5: dispatch the orchestrator flow. Identity/conversation
	// metadata flows through trigger data as new reserved keys that
	// the executor must not auto-wire as action inputs.
	if reg.OrchestratorFlowID != nil {
		if err := s.dispatchExecution(reg, message, msgID, agentUserID, conversationID, conversationHistory); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Error("failed to dispatch agent execution")
			return err
		}
	}

	return nil
}

// deriveExternalID picks the stable external identifier and display
// name for the message sender, per channel type. The external id
// must be stable across renames (Slack U-ids, Telegram numeric ids)
// because it is the lookup key for agent_identity.
func deriveExternalID(msg InboundMessage) (externalID, displayName string) {
	displayName = msg.Sender
	if msg.Metadata == nil {
		return msg.Sender, displayName
	}
	switch msg.ChannelType {
	case "slack":
		if v, ok := msg.Metadata["user_id"].(string); ok && v != "" {
			externalID = v
		}
		if v, ok := msg.Metadata["user_name"].(string); ok && v != "" {
			displayName = v
		} else if v, ok := msg.Metadata["display_name"].(string); ok && v != "" {
			displayName = v
		}
	case "telegram":
		if v, ok := msg.Metadata["sender_id"].(string); ok && v != "" {
			externalID = v
		}
		if v, ok := msg.Metadata["sender_name"].(string); ok && v != "" {
			displayName = v
		} else if v, ok := msg.Metadata["sender_username"].(string); ok && v != "" {
			displayName = "@" + v
		}
	}
	if externalID == "" {
		// Fall back to sender string — not ideal (may rename) but
		// keeps unknown channel types working.
		externalID = msg.Sender
	}
	return externalID, displayName
}

// deriveChannelScope returns the conversation scoping key for a
// message. Slack DMs scope to channel_id, threaded replies scope to
// (channel_id, thread_ts). Telegram scopes to chat_id. Webhooks have
// no stable channel identifier so the caller gets an empty channelID
// and skips conversation resolution entirely.
func deriveChannelScope(msg InboundMessage) (channelID string, threadID *string) {
	if msg.Metadata == nil {
		return "", nil
	}
	switch msg.ChannelType {
	case "slack":
		if v, ok := msg.Metadata["channel_id"].(string); ok {
			channelID = v
		}
		if v, ok := msg.Metadata["thread_ts"].(string); ok && v != "" {
			t := v
			threadID = &t
		}
	case "telegram":
		if v, ok := msg.Metadata["chat_id"].(string); ok {
			channelID = v
		}
	}
	return channelID, threadID
}

// startManaging begins lifecycle management for an agent on this instance.
func (s *Service) startManaging(reg *launch.AgentRegistration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop existing management if re-registering
	if existing, ok := s.managedAgents[reg.AgentID]; ok && !existing.stopped {
		close(existing.stopCh)
	}

	acquired, err := s.db.TryAcquireAgentLease(reg.AgentID, s.instanceID, leaseDuration)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
			"error":    err,
		}).Error("failed to acquire agent lease")
		return
	}

	if !acquired {
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
		}).Debug("agent lease held by another instance, skipping")
		return
	}

	ma := &managedAgent{
		reg:    reg,
		stopCh: make(chan struct{}),
	}
	s.managedAgents[reg.AgentID] = ma
}

// stopManaging stops lifecycle management for an agent.
func (s *Service) stopManaging(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ma, ok := s.managedAgents[agentID]; ok && !ma.stopped {
		close(ma.stopCh)
		ma.stopped = true
		delete(s.managedAgents, agentID)
	}
}

// heartbeatLoop periodically renews leases for all managed agents.
func (s *Service) heartbeatLoop() {
	time.Sleep(startupDelay)

	for {
		s.renewLeases()
		time.Sleep(heartbeatInterval)
	}
}

func (s *Service) renewLeases() {
	s.mu.RLock()
	agents := make([]string, 0, len(s.managedAgents))
	for id := range s.managedAgents {
		agents = append(agents, id)
	}
	s.mu.RUnlock()

	for _, agentID := range agents {
		acquired, err := s.db.TryAcquireAgentLease(agentID, s.instanceID, leaseDuration)
		if err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Warn("failed to renew agent lease")
			continue
		}
		if !acquired {
			// Lost the lease to another instance
			log.WithFields(log.Fields{
				"agent_id":    agentID,
				"instance_id": s.instanceID,
			}).Warn("agent lease lost to another instance")
			s.stopManaging(agentID)
		}

		// Update session heartbeat via API
		s.updateHeartbeat(agentID)
	}
}

// watchdog scans for orphaned agents (active registrations with expired leases)
// and claims them. This enables crash recovery across Launch instances.
func (s *Service) watchdog() {
	time.Sleep(startupDelay)

	// On startup, claim any orphaned agents
	s.claimOrphanedAgents()

	for {
		time.Sleep(watchdogInterval)
		s.claimOrphanedAgents()
	}
}

func (s *Service) claimOrphanedAgents() {
	orphans, err := s.db.GetExpiredAgentLeases()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("watchdog: failed to get expired agent leases")
		return
	}

	for _, reg := range orphans {
		acquired, err := s.db.TryAcquireAgentLease(reg.AgentID, s.instanceID, leaseDuration)
		if err != nil {
			log.WithFields(log.Fields{
				"agent_id": reg.AgentID,
				"error":    err,
			}).Warn("watchdog: failed to acquire orphaned agent lease")
			continue
		}

		if acquired {
			log.WithFields(log.Fields{
				"agent_id":    reg.AgentID,
				"instance_id": s.instanceID,
			}).Info("watchdog: claimed orphaned agent")

			s.mu.Lock()
			s.managedAgents[reg.AgentID] = &managedAgent{
				reg:    reg,
				stopCh: make(chan struct{}),
			}
			s.mu.Unlock()
		}
	}
}

// updateHeartbeat calls the API to refresh the agent session heartbeat.
func (s *Service) updateHeartbeat(agentID string) {
	s.mu.RLock()
	ma, ok := s.managedAgents[agentID]
	s.mu.RUnlock()
	if !ok || ma.reg.APIURL == "" {
		return
	}

	// We update via the API's agent session endpoint
	// For now, this is a no-op until the API exposes a heartbeat endpoint.
	// The session heartbeat_at is updated when messages are processed.
	_ = ma
}

// agentIdentityResponse mirrors the API's resolve-identity response.
type agentIdentityResponse struct {
	Identity struct {
		ID string `json:"id"`
	} `json:"identity"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

// agentConversationResponse mirrors the API's resolve-conversation response.
type agentConversationResponse struct {
	ID string `json:"id"`
}

// resolveIdentity calls the API's internal resolve-identity endpoint to
// look up (or auto-create on first contact) the AgentIdentity +
// AgentUser for the sender of an inbound message.
func (s *Service) resolveIdentity(reg *launch.AgentRegistration, msg InboundMessage) (*agentIdentityResponse, error) {
	if reg.APIURL == "" {
		return nil, fmt.Errorf("agent registration has no api_url")
	}
	externalID, displayName := deriveExternalID(msg)
	if externalID == "" {
		return nil, fmt.Errorf("could not derive external id for %s message", msg.ChannelType)
	}

	body := map[string]interface{}{
		"channel_type":        msg.ChannelType,
		"channel_external_id": externalID,
	}
	if displayName != "" {
		body["display_name"] = displayName
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/resolve-identity", reg.APIURL, reg.AgentID)
	client := http.Client{Timeout: apiTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API returned %d resolving identity: %s", resp.StatusCode, string(rb))
	}

	var result agentIdentityResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// resolveConversation calls the API's internal resolve-conversation
// endpoint to look up (or open) the conversation scoped to the given
// (agent, user, channel, thread) tuple.
func (s *Service) resolveConversation(reg *launch.AgentRegistration, agentUserID *string, channelType, channelID string, threadID *string) (*agentConversationResponse, error) {
	if reg.APIURL == "" {
		return nil, fmt.Errorf("agent registration has no api_url")
	}

	body := map[string]interface{}{
		"channel_type": channelType,
		"channel_id":   channelID,
	}
	if agentUserID != nil {
		body["agent_user_id"] = *agentUserID
	}
	if threadID != nil {
		body["thread_id"] = *threadID
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/conversation", reg.APIURL, reg.AgentID)
	client := http.Client{Timeout: apiTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API returned %d resolving conversation: %s", resp.StatusCode, string(rb))
	}

	var result agentConversationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// fetchConversationHistory pulls the last N turns for a conversation
// from the API. Returns nil on any error — the caller treats an empty
// history as "fresh conversation" rather than failing.
func (s *Service) fetchConversationHistory(reg *launch.AgentRegistration, conversationID string, limit int) []map[string]interface{} {
	if reg.APIURL == "" || conversationID == "" {
		return nil
	}

	url := fmt.Sprintf("%s/api/v1/internal/conversation/%s/history?limit=%d", reg.APIURL, conversationID, limit)
	client := http.Client{Timeout: apiTimeout}
	resp, err := client.Get(url)
	if err != nil {
		log.WithFields(log.Fields{
			"conversation_id": conversationID,
			"error":           err,
		}).Warn("failed to fetch conversation history")
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var messages []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil
	}
	return messages
}

// storeMessage records an inbound message via the API. When
// conversationID is supplied the message is written into the
// conversation-scoped endpoint (which assigns a sequence number);
// otherwise it falls back to the legacy agent-wide endpoint so callers
// with no resolvable channel scope (e.g. generic webhooks) still work.
func (s *Service) storeMessage(reg *launch.AgentRegistration, msg InboundMessage, conversationID *string) (*string, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"direction":    "inbound",
		"channel_type": msg.ChannelType,
		"sender":       msg.Sender,
		"content":      msg.Content,
		"metadata":     msg.Metadata,
	})
	if err != nil {
		return nil, err
	}

	var url string
	if conversationID != nil {
		url = fmt.Sprintf("%s/api/v1/internal/conversation/%s/message?agent_id=%s", reg.APIURL, *conversationID, reg.AgentID)
	} else {
		url = fmt.Sprintf("%s/api/v1/internal/agent/%s/message", reg.APIURL, reg.AgentID)
	}
	client := http.Client{Timeout: apiTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("API returned %d storing message", resp.StatusCode)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result.ID, nil
}

// dispatchExecution triggers the agent's orchestrator flow via the API.
// Identity, conversation, and history metadata flow through trigger
// data as reserved keys (agent_id, agent_user_id, conversation_id,
// conversation_history, system_prompt) that the executor must not
// auto-wire as raw action inputs.
func (s *Service) dispatchExecution(
	reg *launch.AgentRegistration,
	msg InboundMessage,
	msgID *string,
	agentUserID *string,
	conversationID *string,
	conversationHistory []map[string]interface{},
) error {
	if reg.OrchestratorFlowID == nil {
		return nil
	}

	// Build trigger data with message content.
	// Promote metadata fields to top level so they're accessible as ${trigger.chat_id} etc.
	data := map[string]interface{}{
		"agent_id":     reg.AgentID,
		"channel_type": msg.ChannelType,
		"sender":       msg.Sender,
		"content":      msg.Content,
	}
	if reg.SystemPrompt != nil && *reg.SystemPrompt != "" {
		data["system_prompt"] = *reg.SystemPrompt
	}
	if msgID != nil {
		data["message_id"] = *msgID
	}
	if agentUserID != nil {
		data["agent_user_id"] = *agentUserID
	}
	if conversationID != nil {
		data["conversation_id"] = *conversationID
	}
	if conversationHistory != nil {
		data["conversation_history"] = conversationHistory
	}
	// Flatten metadata into trigger data for direct variable access
	for k, v := range msg.Metadata {
		if _, exists := data[k]; !exists {
			data[k] = v
		}
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	// Use internal endpoints (no auth required) for service-to-service calls
	var url string
	if reg.TriggerID != nil {
		url = fmt.Sprintf("%s/api/v1/internal/flo/%s/trigger/%s/execute",
			reg.APIURL, *reg.OrchestratorFlowID, *reg.TriggerID)
	} else {
		url = fmt.Sprintf("%s/api/v1/internal/flo/%s/execute",
			reg.APIURL, *reg.OrchestratorFlowID)
	}

	client := http.Client{Timeout: apiTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to trigger execution: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("API returned %d triggering execution: %s", resp.StatusCode, string(body))
	}

	hasTrigger := reg.TriggerID != nil
	log.WithFields(log.Fields{
		"agent_id":    reg.AgentID,
		"flow_id":     *reg.OrchestratorFlowID,
		"sender":      msg.Sender,
		"has_trigger": hasTrigger,
		"url":         url,
	}).Info("agent execution dispatched")

	return nil
}

// activateChannels sets up external channel integrations for an agent.
func (s *Service) activateChannels(reg *launch.AgentRegistration) {
	var channels []struct {
		Type   string          `json:"type"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(reg.Channels, &channels); err != nil {
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
			"error":    err,
		}).Warn("unable to parse agent channels")
		return
	}

	for _, ch := range channels {
		switch ch.Type {
		case "telegram":
			var cfg struct {
				BotToken string `json:"bot_token"`
			}
			if err := json.Unmarshal(ch.Config, &cfg); err != nil || cfg.BotToken == "" {
				log.WithFields(log.Fields{
					"agent_id": reg.AgentID,
				}).Warn("telegram channel missing bot_token")
				continue
			}
			if err := s.telegram.RegisterWebhook(reg.AgentID, cfg.BotToken); err != nil {
				log.WithFields(log.Fields{
					"agent_id": reg.AgentID,
					"error":    err,
				}).Error("failed to register telegram webhook")
			}
		// Future: case "email" — start IMAP polling
		}
	}
}

// deactivateChannels tears down external channel integrations for an agent.
func (s *Service) deactivateChannels(agentID string) {
	if err := s.telegram.DeregisterWebhook(agentID); err != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
			"error":    err,
		}).Warn("failed to deregister telegram webhook")
	}
}

// InboundMessage represents a message received from an external channel.
type InboundMessage struct {
	ChannelType string                 `json:"channel_type"`
	Sender      string                 `json:"sender"`
	Content     string                 `json:"content"`
	Metadata    map[string]interface{} `json:"metadata"`
}
