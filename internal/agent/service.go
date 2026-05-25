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
	"strings"
	"sync"
	"time"

	"context"

	"strconv"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	appmetrics "flomation.app/automate/launch/internal/metrics"
	"flomation.app/automate/launch/internal/mtls"
	"flomation.app/automate/launch/internal/persistence"

	slackPkg "flomation.app/automate/launch/internal/slack"
	telegramPkg "flomation.app/automate/launch/internal/telegram"
	"flomation.app/automate/launch/internal/trigger"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	leaseDuration     = 30 * time.Second
	heartbeatInterval = 10 * time.Second
	watchdogInterval  = 15 * time.Second
	startupDelay      = 2 * time.Second
	apiTimeout        = 30 * time.Second
)

// Service manages agent runtimes within the Launch process.
// Each Launch instance generates a unique instanceID and uses lease-based
// ownership to coordinate with other instances.
type Service struct {
	config       *config.Config
	db           *persistence.Service
	trigger      *trigger.Service
	telegram     *telegramPkg.Service
	slackSockets *slackPkg.SocketManager
	facebook     FacebookChannelManager // nil until SetFacebookManager is called
	embedding    embeddingProvider      // nil when embeddings are disabled
	apiClient    *http.Client           // mTLS-capable client for API calls
	instanceID   string

	mu            sync.RWMutex
	managedAgents map[string]*managedAgent // agentID → active management state
}

// embeddingProvider is the subset of embedding.Provider that the agent
// service needs. Defined as a local interface to avoid importing the
// embedding package in the service struct (keeps test mocking simple).
type embeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// FacebookChannelManager provides agent channel registration/deregistration
// for Facebook Messenger. Implemented by the HTTP service's PageIndex.
type FacebookChannelManager interface {
	AddAgent(pageID, agentID string)
	RemoveAgent(agentID string)
}

// managedAgent tracks runtime state for an agent this instance is managing.
type managedAgent struct {
	reg     *launch.AgentRegistration
	stopCh  chan struct{}
	stopped bool
}

// SetFacebookManager injects the Facebook channel manager after construction.
// Called by the HTTP service once the PageIndex is available.
func (s *Service) SetFacebookManager(mgr FacebookChannelManager) {
	s.facebook = mgr
}

// NewService creates and starts the agent orchestration service.
func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service, telegramSvc *telegramPkg.Service, embed embeddingProvider) *Service {
	apiClient, err := mtls.ClientOrDefault(config.TLS, apiTimeout)
	if err != nil {
		log.WithError(err).Fatal("agent: unable to create API client")
	}

	s := &Service{
		config:        config,
		db:            db,
		trigger:       trigger,
		telegram:      telegramSvc,
		slackSockets:  slackPkg.NewSocketManager(),
		embedding:     embed,
		apiClient:     apiClient,
		instanceID:    uuid.New().String(),
		managedAgents: make(map[string]*managedAgent),
	}

	go s.watchdog()
	go s.heartbeatLoop()
	s.startEmbeddingBackfill()

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
	appmetrics.AgentsManaged.Inc()

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
	appmetrics.AgentsManaged.Dec()

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

	// Send typing indicator BEFORE dispatching to API — it must arrive
	// before the orchestrator starts processing. This is the one thing
	// that stays in Launch because it needs the Telegram SDK.
	if message.ChannelType == "telegram" || message.ChannelType == "telegram_voice" {
		s.sendTypingIndicator(reg, message)
	}

	// Phase 3: try the new single API endpoint first. Falls back to
	// the legacy 7-step pipeline on any error.
	if err := s.handleInboundViaAPI(reg, message); err != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
			"error":    err,
		}).Warn("API inbound endpoint failed — falling back to legacy pipeline")
		return s.handleInboundMessageForReg(reg, message)
	}
	return nil
}

// handleInboundViaAPI calls the API's single inbound-message endpoint.
func (s *Service) handleInboundViaAPI(reg *launch.AgentRegistration, msg InboundMessage) error {
	if s.config.InternalAPIURL() == "" {
		return fmt.Errorf("no api_url configured")
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/api/v1/internal/agent/%s/inbound-message",
		s.config.InternalAPIURL(), reg.AgentID)

	resp, err := s.apiClient.Post(endpoint, "application/json", bytes.NewReader(payload)) // #nosec G107
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("API returned %d: %s", resp.StatusCode, string(rb))
	}

	log.WithFields(log.Fields{
		"agent_id": reg.AgentID,
		"sender":   msg.Sender,
	}).Info("inbound message processed via API endpoint")
	return nil
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

			// If a stale conversation was closed, generate a session
			// summary in the background. This captures the context of
			// the previous conversation as a memory before starting fresh.
			if conv.ClosedConversationID != nil {
				go s.generateSessionSummary(reg, *conv.ClosedConversationID, agentUserID)
			}
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

	// Step 4b: typing indicator is now sent in HandleInboundMessage
	// before either the API or legacy path, so we skip it here to
	// avoid sending it twice on the fallback path.

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

	// Step 6 (Phase 2d-γ): fire the extraction pipeline. This runs in
	// parallel with (or after) the orchestrator flow and never blocks
	// the reply path — any error here is logged and swallowed. The
	// endpoint is a 204 no-op for agents without an extraction flow
	// configured (Phase 2d-α's design), so calling it unconditionally
	// is safe.
	s.dispatchExtraction(reg, message, msgID, agentUserID, conversationID, "user")

	// Step 7 (Phase 5): check if this message is an affirmative reply
	// to a pending action confirmation. When the pending action poller
	// dispatches a confirmation prompt, the user may reply with a short
	// "yes"/"sure"/"go ahead". Rather than relying on extraction to
	// detect this, we check directly: if the user has an open, notified
	// pending action and their message is affirmative, transition it.
	if agentUserID != nil && *agentUserID != "" {
		s.checkPendingActionConfirmation(reg, message, *agentUserID)
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
	case "email":
		// Use the bare email address as the stable external ID.
		// The "from" field often includes a display name in the format
		// "Name <address>" — extract just the address part.
		if v, ok := msg.Metadata["from"].(string); ok && v != "" {
			externalID = extractBareEmail(v)
			displayName = v
		}
	case "facebook_messenger":
		// Messenger PSID is a stable per-page user identifier.
		if v, ok := msg.Metadata["user_id"].(string); ok && v != "" {
			externalID = v
		}
		if v, ok := msg.Metadata["user_name"].(string); ok && v != "" {
			displayName = v
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
	case "email":
		// For conversation scoping, use the Gmail account email.
		// channel_id in trigger data holds the email_id (for ${flow.channel_id}),
		// so we use "account" for the conversation scope instead.
		if v, ok := msg.Metadata["account"].(string); ok {
			channelID = v
		}
		if v, ok := msg.Metadata["thread_id"].(string); ok && v != "" {
			t := v
			threadID = &t
		}
	case "facebook_messenger":
		// Messenger conversations are 1:1 per PSID — use PSID as scope.
		if v, ok := msg.Metadata["user_id"].(string); ok {
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
		s.reconnectStaleChannels()
		time.Sleep(heartbeatInterval)
	}
}

// reconnectStaleChannels checks for Socket Mode connections that have
// silently died and re-establishes them. This prevents agents from going
// deaf when the WebSocket drops without a clean shutdown.
func (s *Service) reconnectStaleChannels() {
	if n := s.slackSockets.ReconnectStale(); n > 0 {
		log.WithField("count", n).Info("reconnected stale slack socket mode connections")
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

	// On startup, expire all leases so this instance can claim agents
	// immediately rather than waiting for the previous instance's lease
	// to time out. Safe because no agents are managed yet at this point.
	if err := s.db.ExpireAllAgentLeases(); err != nil {
		log.WithError(err).Warn("watchdog: failed to expire leases on startup")
	}

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

			s.activateChannels(reg)
		}
	}
}

// updateHeartbeat calls the API to refresh the agent session heartbeat.
func (s *Service) updateHeartbeat(agentID string) {
	s.mu.RLock()
	ma, ok := s.managedAgents[agentID]
	s.mu.RUnlock()
	if !ok || s.config.InternalAPIURL() == "" {
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
	ID                   string  `json:"id"`
	ClosedConversationID *string `json:"closed_conversation_id,omitempty"`
}

// resolveIdentity calls the API's internal resolve-identity endpoint to
// look up (or auto-create on first contact) the AgentIdentity +
// AgentUser for the sender of an inbound message.
func (s *Service) resolveIdentity(reg *launch.AgentRegistration, msg InboundMessage) (*agentIdentityResponse, error) {
	if s.config.InternalAPIURL() == "" {
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

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/resolve-identity", s.config.InternalAPIURL(), reg.AgentID)
	resp, err := s.apiClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

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
	if s.config.InternalAPIURL() == "" {
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

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/conversation", s.config.InternalAPIURL(), reg.AgentID)
	resp, err := s.apiClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

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
	if s.config.InternalAPIURL() == "" || conversationID == "" {
		return nil
	}

	url := fmt.Sprintf("%s/api/v1/internal/conversation/%s/history?limit=%d", s.config.InternalAPIURL(), conversationID, limit)
	resp, err := s.apiClient.Get(url)
	if err != nil {
		log.WithFields(log.Fields{
			"conversation_id": conversationID,
			"error":           err,
		}).Warn("failed to fetch conversation history")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var messages []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil
	}
	// Normalise direction → role so the AI action recognises entries
	// as conversation turns. The API returns agent_message rows with
	// direction=inbound/outbound/tool_use/tool_result, but the
	// Anthropic/OpenAI actions expect role=user/assistant.
	//
	// Tool messages are converted to text summaries that
	// ParseConversationHistory can pass through. The AI sees a
	// human-readable record of what it called and what came back,
	// which gives it continuity across turns.
	var normalised []map[string]interface{}
	for _, msg := range messages {
		dir, _ := msg["direction"].(string)
		switch dir {
		case "inbound":
			msg["role"] = "user"
			normalised = append(normalised, msg)
		case "outbound":
			msg["role"] = "assistant"
			normalised = append(normalised, msg)
		case "tool_use", "tool_result":
			// Skip tool exchange messages from conversation history.
			// These are internal mechanics within a single AI turn —
			// the final outbound message already summarises what
			// happened. Including them as user/assistant text confuses
			// the model into thinking the user said tool results.
			continue
		default:
			msg["role"] = "user"
			normalised = append(normalised, msg)
		}
	}
	return normalised
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
		url = fmt.Sprintf("%s/api/v1/internal/conversation/%s/message?agent_id=%s", s.config.InternalAPIURL(), *conversationID, reg.AgentID)
	} else {
		url = fmt.Sprintf("%s/api/v1/internal/agent/%s/message", s.config.InternalAPIURL(), reg.AgentID)
	}
	resp, err := s.apiClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

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
		"agent_id":       reg.AgentID,
		"channel_type":   msg.ChannelType,
		"sender":         msg.Sender,
		"content":        msg.Content,
		"trigger_source": "channel",
	}
	// Phase 2b: system_prompt is now the server-assembled view
	// (persona + honesty directive + pinned memories + channel directive
	// + any open pending confirmations) rather than the raw persona
	// string from the agent registration. See internal/agent/system_prompt.go.
	// The assembler fails open — on any fetch error the assembled string
	// degrades to persona + directives with no memory context, so the
	// reply path is never blocked on memory I/O.
	if assembled := s.assembleSystemPromptViaAPI(reg, msg, agentUserID); assembled != "" {
		data["system_prompt"] = assembled
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
			s.config.InternalAPIURL(), *reg.OrchestratorFlowID, *reg.TriggerID)
	} else {
		url = fmt.Sprintf("%s/api/v1/internal/flo/%s/execute",
			s.config.InternalAPIURL(), *reg.OrchestratorFlowID)
	}

	resp, err := s.apiClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to trigger execution: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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

// dispatchExtraction fires the extraction System Flow for this turn.
// Phase 2d-γ: called from handleInboundMessageForReg after the main
// orchestrator dispatch, and from Phase 2d-γ-executor on the
// assistant-reply side via a sibling call in ai_common.
//
// The call is best-effort: any failure (network, non-2xx, malformed
// response) logs a warning and returns. The reply path is never
// blocked on extraction — the user gets their answer even when
// extraction is degraded, and the next turn just doesn't see the
// memories this turn might have produced. This matches the plan's
// "latency on the main path is sacred" principle.
//
// When the agent has no extraction_flow_id configured (the default
// state before the Phase 2d-γ-api seed migration backfills it), the
// API responds with 204 No Content. That's the expected happy path
// for un-bootstrapped agents — we log nothing and move on.
func (s *Service) dispatchExtraction(
	reg *launch.AgentRegistration,
	msg InboundMessage,
	msgID *string,
	agentUserID *string,
	conversationID *string,
	role string,
) {
	if s.config.InternalAPIURL() == "" || reg.AgentID == "" {
		return
	}

	// Build enriched content: for messages under 80 chars, include recent
	// conversation history so the extraction AI can determine context.
	// This covers confirmations ("yes"/"no"), task completions ("never mind,
	// I've done it"), and other short replies that need conversational context.
	enrichedContent := msg.Content
	if conversationID != nil && len(msg.Content) < 80 {
		if history := s.fetchConversationHistory(reg, *conversationID, 4); len(history) > 0 {
			var sb strings.Builder
			sb.WriteString("Recent conversation:\n")
			for _, turn := range history {
				r, _ := turn["role"].(string)
				c, _ := turn["content"].(string)
				if r != "" && c != "" {
					fmt.Fprintf(&sb, "[%s]: %s\n", r, c)
				}
			}
			sb.WriteString("\nCurrent message: ")
			sb.WriteString(msg.Content)
			enrichedContent = sb.String()
		}
	}

	body := map[string]interface{}{
		"role":    role,
		"content": enrichedContent,
	}
	if msgID != nil {
		body["message_id"] = *msgID
	}
	if agentUserID != nil {
		body["agent_user_id"] = *agentUserID
	}
	if conversationID != nil {
		body["conversation_id"] = *conversationID
	}

	payload, err := json.Marshal(body)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
			"error":    err,
		}).Warn("failed to marshal extraction payload")
		return
	}

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/extract", s.config.InternalAPIURL(), reg.AgentID)
	resp, err := s.apiClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
			"error":    err,
		}).Warn("failed to call extract endpoint")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// 204 = no extraction flow configured (the common un-bootstrapped
	// case); 202 = extraction dispatched; anything else is a warning.
	switch resp.StatusCode {
	case http.StatusNoContent:
		// Silent success — agent has no extraction flow set yet.
		return
	case http.StatusAccepted:
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
			"role":     role,
		}).Debug("extraction dispatched")
		return
	default:
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.WithFields(log.Fields{
			"agent_id": reg.AgentID,
			"status":   resp.StatusCode,
			"body":     string(rb),
		}).Warn("unexpected response from extract endpoint")
	}
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
		case "slack":
			var cfg struct {
				BotToken string `json:"bot_token"`
				AppToken string `json:"app_token"`
				Mode     string `json:"mode"` // "socket" or "events_api" (default)
			}
			if err := json.Unmarshal(ch.Config, &cfg); err != nil {
				continue
			}
			if cfg.Mode == "socket" && cfg.AppToken != "" && cfg.BotToken != "" {
				agentID := reg.AgentID
				onMessage := func(msg *slackPkg.ParsedMessage) {
					s.handleSlackSocketMessage(agentID, cfg.BotToken, msg)
				}
				onInteract := func(payload *slackPkg.InteractionPayload) {
					s.handleSlackSocketInteraction(agentID, cfg.BotToken, payload)
				}
				if err := s.slackSockets.Connect(agentID, cfg.AppToken, cfg.BotToken, onMessage, onInteract); err != nil {
					log.WithFields(log.Fields{
						"agent_id": reg.AgentID,
						"error":    err,
					}).Error("failed to start slack socket mode")
				}
			}
		case "facebook_messenger":
			var cfg struct {
				PageID          string `json:"page_id"`
				PageAccessToken string `json:"page_access_token"`
			}
			if err := json.Unmarshal(ch.Config, &cfg); err != nil || cfg.PageID == "" {
				log.WithFields(log.Fields{
					"agent_id": reg.AgentID,
				}).Warn("facebook_messenger channel missing page_id")
				continue
			}
			if s.facebook != nil {
				s.facebook.AddAgent(cfg.PageID, reg.AgentID)
				log.WithFields(log.Fields{
					"agent_id": reg.AgentID,
					"page_id":  cfg.PageID,
				}).Info("Facebook Messenger agent channel activated")
			}
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
	s.slackSockets.Disconnect(agentID)
	if s.facebook != nil {
		s.facebook.RemoveAgent(agentID)
	}
}

// InboundMessage represents a message received from an external channel.
type InboundMessage struct {
	ChannelType string                 `json:"channel_type"`
	Sender      string                 `json:"sender"`
	Content     string                 `json:"content"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// EmailAgentInfo holds the info needed by the email polling service to
// dispatch inbound emails to agents with email channels.
type EmailAgentInfo struct {
	AgentID string
}

// GetAgentsWithEmailChannel returns all managed agents that have an
// email channel configured. Called by the email polling service.
func (s *Service) GetAgentsWithEmailChannel() []EmailAgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []EmailAgentInfo
	for agentID, ma := range s.managedAgents {
		if ma.stopped || ma.reg == nil || ma.reg.Channels == nil {
			continue
		}
		var channels []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(ma.reg.Channels, &channels); err != nil {
			continue
		}
		for _, ch := range channels {
			if ch.Type == "email" {
				result = append(result, EmailAgentInfo{AgentID: agentID})
				break
			}
		}
	}
	return result
}

// sendTypingIndicator sends a typing action to the channel. Runs in a
// goroutine — failures are logged and swallowed.
func (s *Service) sendTypingIndicator(reg *launch.AgentRegistration, msg InboundMessage) {
	l := log.WithFields(log.Fields{
		"agent_id":     reg.AgentID,
		"channel_type": msg.ChannelType,
	})

	chatID := ""
	if msg.Metadata != nil {
		if v, ok := msg.Metadata["chat_id"].(string); ok {
			chatID = v
		}
	}
	if chatID == "" {
		l.Warn("typing indicator: no chat_id in metadata")
		return
	}
	l = l.WithField("chat_id", chatID)

	// Extract bot token from agent channel config.
	var channels []struct {
		Type   string `json:"type"`
		Config struct {
			BotToken string `json:"bot_token"`
		} `json:"config"`
	}
	if err := json.Unmarshal(reg.Channels, &channels); err != nil {
		l.WithError(err).Warn("typing indicator: failed to parse channels config")
		return
	}
	botToken := ""
	for _, ch := range channels {
		if ch.Type == "telegram" && ch.Config.BotToken != "" {
			botToken = ch.Config.BotToken
			break
		}
	}
	if botToken == "" {
		l.Warn("typing indicator: no telegram bot token found in channels config")
		return
	}

	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		l.WithError(err).Warn("typing indicator: failed to parse chat_id as int64")
		return
	}

	l.Info("typing indicator: sending typing action")
	if err := telegramPkg.SendChatAction(botToken, chatIDInt, "typing"); err != nil {
		l.WithError(err).Warn("typing indicator: Telegram API call failed")
	} else {
		l.Info("typing indicator: sent successfully")
	}
}

// extractBareEmail extracts the bare email address from a "Name <address>"
// generateSessionSummary fetches the full history of a closed conversation
// and dispatches a summary extraction. The extraction pipeline creates a
// session_summary memory with valid_until set to 30 days (temporal decay).
func (s *Service) generateSessionSummary(reg *launch.AgentRegistration, closedConvID string, agentUserID *string) {
	l := log.WithFields(log.Fields{
		"agent_id":        reg.AgentID,
		"conversation_id": closedConvID,
	})

	// Fetch full conversation history (up to 50 turns).
	history := s.fetchConversationHistory(reg, closedConvID, 50)
	if len(history) == 0 {
		l.Debug("no history for closed conversation, skipping summary")
		return
	}

	// Build a summary prompt from the conversation.
	var sb strings.Builder
	sb.WriteString("Summarise this completed conversation in 2-3 sentences. ")
	sb.WriteString("Focus on: what the user asked for, what was accomplished, ")
	sb.WriteString("and any outstanding items. Write as a factual summary, not as a message.\n\n")
	for _, turn := range history {
		role, _ := turn["role"].(string)
		content, _ := turn["content"].(string)
		if role != "" && content != "" {
			fmt.Fprintf(&sb, "[%s]: %s\n", role, content)
		}
	}

	// Dispatch to the extraction pipeline with role=summary.
	// The extraction prompt handles this by creating a session_summary memory.
	body := map[string]interface{}{
		"role":    "summary",
		"content": sb.String(),
	}
	if agentUserID != nil {
		body["agent_user_id"] = *agentUserID
	}

	payload, err := json.Marshal(body)
	if err != nil {
		l.WithError(err).Warn("failed to marshal summary extraction")
		return
	}

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/extract", s.config.InternalAPIURL(), reg.AgentID)
	resp, err := s.apiClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		l.WithError(err).Warn("failed to dispatch session summary extraction")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusAccepted {
		l.Info("session summary extraction dispatched for closed conversation")
	}
}

// format string. If the input doesn't contain angle brackets, it's returned
// as-is (assumed to already be a bare address).
func extractBareEmail(from string) string {
	start := strings.Index(from, "<")
	end := strings.Index(from, ">")
	if start >= 0 && end > start {
		return strings.TrimSpace(from[start+1 : end])
	}
	return strings.TrimSpace(from)
}

// extractEmailBody strips email headers and quoted reply text, returning
// just the new body content. Handles the format:
// "New email from Name <addr>\nSubject: ...\n\nBody\r\n\r\nOn ... wrote:\r\n> quoted"
func extractEmailBody(content string) string {
	// Find the first blank line (separates headers from body).
	// Headers are "New email from...", "Subject: ...", etc.
	body := content
	if idx := strings.Index(content, "\n\n"); idx >= 0 {
		body = content[idx+2:]
	} else if idx := strings.Index(content, "\r\n\r\n"); idx >= 0 {
		body = content[idx+4:]
	}

	// Strip quoted reply (lines starting with > or "On ... wrote:")
	lines := strings.Split(body, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Stop at quoted text marker
		if strings.HasPrefix(trimmed, "On ") && strings.Contains(trimmed, " wrote:") {
			break
		}
		if strings.HasPrefix(trimmed, ">") {
			break
		}
		if trimmed != "" {
			// Strip trailing \r
			result = append(result, strings.TrimRight(trimmed, "\r"))
		}
	}

	return strings.Join(result, " ")
}

// affirmatives is a set of short replies that indicate confirmation.
var affirmatives = map[string]bool{
	"yes":        true,
	"yes please": true,
	"yep":        true,
	"yeah":       true,
	"sure":       true,
	"go ahead":   true,
	"confirm":    true,
	"confirmed":  true,
	"do it":      true,
	"link them":  true,
	"link it":    true,
	"ok":         true,
	"okay":       true,
	"y":          true,
}

// decliners is a set of short replies that indicate declining.
var decliners = map[string]bool{
	"no":       true,
	"nope":     true,
	"nah":      true,
	"don't":    true,
	"cancel":   true,
	"decline":  true,
	"declined": true,
	"n":        true,
}

// checkPendingActionConfirmation checks if the user's message is a short
// affirmative/negative reply to a pending action that has been notified.
// If so, it updates the pending action status directly — bypassing the
// extraction pipeline which is unreliable for short replies.
func (s *Service) checkPendingActionConfirmation(
	reg *launch.AgentRegistration,
	msg InboundMessage,
	agentUserID string,
) {
	// For email messages, extract just the body text (strip headers,
	// quoted replies, and signatures). Email content arrives as:
	// "New email from Name <addr>\nSubject: ...\n\nBody\r\n\r\nOn ... wrote:\r\n> quoted"
	content := msg.Content
	if msg.ChannelType == "email" {
		content = extractEmailBody(content)
	}

	// Only check short messages — long messages are unlikely to be
	// simple confirmations.
	normalised := strings.TrimSpace(strings.ToLower(content))
	if len(normalised) > 30 {
		return
	}

	isConfirm := affirmatives[normalised]
	isDecline := decliners[normalised]
	if !isConfirm && !isDecline {
		return
	}

	// Fetch open pending actions for this user.
	endpoint := fmt.Sprintf(
		"%s/api/v1/internal/agent/%s/pending-action?agent_user_id=%s",
		s.config.InternalAPIURL(), reg.AgentID, agentUserID,
	)
	resp, err := s.apiClient.Get(endpoint)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}

	var actions []struct {
		ID         string     `json:"id"`
		Type       string     `json:"type"`
		Status     string     `json:"status"`
		NotifiedAt *time.Time `json:"notified_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&actions); err != nil {
		return
	}

	// Find the most recent notified pending action.
	for _, pa := range actions {
		if pa.NotifiedAt == nil {
			continue // Only match actions that the poller has asked about.
		}
		if pa.Status != "awaiting_confirmation" {
			continue
		}

		newStatus := "declined"
		if isConfirm {
			newStatus = "confirmed_here_awaiting_other_side"
		}

		// PATCH the status.
		body, _ := json.Marshal(map[string]string{"status": newStatus})
		patchURL := fmt.Sprintf("%s/api/v1/internal/pending-action/%s", s.config.InternalAPIURL(), pa.ID)
		req, err := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		patchResp, err := s.apiClient.Do(req)
		if err != nil {
			continue
		}
		_ = patchResp.Body.Close()

		l := log.WithFields(log.Fields{
			"agent_id":          reg.AgentID,
			"pending_action_id": pa.ID,
			"type":              pa.Type,
			"resolution":        newStatus,
		})

		if patchResp.StatusCode == http.StatusNoContent {
			l.Info("pending action resolved via short reply detection")

			if isConfirm {
				switch pa.Type {
				case "identity_link":
					// First-side confirmed — trigger cross-channel verification.
					go s.triggerCrossChannelVerification(reg, pa.ID, agentUserID)
				case "identity_link_verification":
					// Second-side confirmed — trigger the identity merge.
					go s.triggerIdentityMerge(reg, pa.ID)
				}
			}
		} else {
			l.Warn("failed to update pending action status")
		}

		// Only process the first matching pending action.
		break
	}
}

// triggerCrossChannelVerification dispatches the identity verification
// request to the API, which in turn creates a target-side pending action
// and dispatches to Launch for cross-channel confirmation.
func (s *Service) triggerCrossChannelVerification(
	reg *launch.AgentRegistration,
	pendingActionID string,
	agentUserID string,
) {
	body, _ := json.Marshal(map[string]interface{}{
		"pending_action_id":   pendingActionID,
		"source_user_id":      agentUserID,
		"source_channel_type": "unknown",
	})

	endpoint := fmt.Sprintf(
		"%s/api/v1/internal/agent/%s/identity/request-verification",
		s.config.InternalAPIURL(), reg.AgentID,
	)

	resp, err := s.apiClient.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id":          reg.AgentID,
			"pending_action_id": pendingActionID,
			"error":             err,
		}).Warn("failed to trigger cross-channel verification")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	log.WithFields(log.Fields{
		"agent_id":          reg.AgentID,
		"pending_action_id": pendingActionID,
		"status_code":       resp.StatusCode,
	}).Info("cross-channel verification triggered")
}

// triggerIdentityMerge is called when the second side of an identity link
// confirms. It fetches the verification PA's payload to get the source and
// target user IDs, marks the original PA as executed, and calls the merge
// endpoint to unify the two agent_user records.
func (s *Service) triggerIdentityMerge(reg *launch.AgentRegistration, verificationPAID string) {
	l := log.WithFields(log.Fields{
		"agent_id":           reg.AgentID,
		"verification_pa_id": verificationPAID,
	})

	// Fetch the verification PA to get payload (source_user_id, target_user_id, original_pa_id).
	paURL := fmt.Sprintf("%s/api/v1/internal/pending-action/%s", s.config.InternalAPIURL(), verificationPAID)
	resp, err := s.apiClient.Get(paURL)
	if err != nil {
		l.WithError(err).Warn("failed to fetch verification PA for merge")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		l.Warn("verification PA not found")
		return
	}

	var pa struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pa); err != nil {
		l.WithError(err).Warn("failed to decode verification PA")
		return
	}

	var payload struct {
		SourceUserID string `json:"source_user_id"`
		TargetUserID string `json:"target_user_id"`
		OriginalPAID string `json:"original_pa_id"`
	}
	if err := json.Unmarshal(pa.Payload, &payload); err != nil {
		l.WithError(err).Warn("failed to decode verification PA payload")
		return
	}

	if payload.SourceUserID == "" || payload.TargetUserID == "" {
		l.Warn("verification PA payload missing user IDs")
		return
	}

	// Mark the original PA as executed.
	if payload.OriginalPAID != "" {
		statusBody, _ := json.Marshal(map[string]string{"status": "executed"})
		patchURL := fmt.Sprintf("%s/api/v1/internal/pending-action/%s", s.config.InternalAPIURL(), payload.OriginalPAID)
		req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(statusBody))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
			r, err := s.apiClient.Do(req)
			if err == nil {
				_ = r.Body.Close()
			}
		}
	}

	// Mark the verification PA as executed.
	statusBody, _ := json.Marshal(map[string]string{"status": "executed"})
	patchURL := fmt.Sprintf("%s/api/v1/internal/pending-action/%s", s.config.InternalAPIURL(), verificationPAID)
	req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(statusBody))
	if req != nil {
		req.Header.Set("Content-Type", "application/json")
		r, err := s.apiClient.Do(req)
		if err == nil {
			_ = r.Body.Close()
		}
	}

	// Call the merge endpoint.
	mergeBody, _ := json.Marshal(map[string]string{
		"source_user_id": payload.SourceUserID,
		"target_user_id": payload.TargetUserID,
	})
	mergeURL := fmt.Sprintf("%s/api/v1/internal/agent/%s/identity/merge", s.config.InternalAPIURL(), reg.AgentID)
	mergeResp, err := s.apiClient.Post(mergeURL, "application/json", bytes.NewReader(mergeBody))
	if err != nil {
		l.WithError(err).Warn("failed to call identity merge")
		return
	}
	defer func() { _ = mergeResp.Body.Close() }()

	if mergeResp.StatusCode == http.StatusOK {
		l.Info("identity merge completed successfully")
	} else {
		l.WithField("status_code", mergeResp.StatusCode).Warn("identity merge returned non-200")
	}
}
