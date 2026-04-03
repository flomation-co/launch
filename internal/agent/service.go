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

// HandleInboundMessage processes an incoming message for an agent.
// This is called from HTTP webhook handlers and is stateless — any instance can handle it.
func (s *Service) HandleInboundMessage(agentID string, message InboundMessage) error {
	reg, err := s.db.GetAgentRegistration(agentID)
	if err != nil {
		return fmt.Errorf("failed to get agent registration: %w", err)
	}
	if reg == nil || reg.DisabledAt != nil {
		return fmt.Errorf("agent %s is not registered or is disabled", agentID)
	}

	// Store message via API
	msgID, err := s.storeMessage(reg, message)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
			"error":    err,
		}).Error("failed to store agent message")
		// Continue to dispatch even if message storage fails
	}

	// Dispatch execution if orchestrator flow is configured
	if reg.OrchestratorFlowID != nil {
		if err := s.dispatchExecution(reg, message, msgID); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Error("failed to dispatch agent execution")
			return err
		}
	}

	return nil
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

// storeMessage records an inbound message via the API.
func (s *Service) storeMessage(reg *launch.AgentRegistration, msg InboundMessage) (*string, error) {
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

	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/message", reg.APIURL, reg.AgentID)
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
func (s *Service) dispatchExecution(reg *launch.AgentRegistration, msg InboundMessage, msgID *string) error {
	if reg.OrchestratorFlowID == nil {
		return nil
	}

	// Build trigger data with message content
	data := map[string]interface{}{
		"agent_id":     reg.AgentID,
		"channel_type": msg.ChannelType,
		"sender":       msg.Sender,
		"content":      msg.Content,
		"metadata":     msg.Metadata,
	}
	if msgID != nil {
		data["message_id"] = *msgID
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
