// Package commitment implements the Phase 3 commitment poller — the
// "proactive clock" in the two-clock architecture described in
// plans/agent_memory.md §"Commitments" and §"Phase 3".
//
// Every 30 seconds, the poller:
//  1. Fetches due commitments (status='pending', due_at <= NOW()) from
//     the API via the Phase 2a GET /internal/commitment/due endpoint.
//  2. Claims each one by PATCHing status → 'firing'.
//  3. Looks up the agent's registration to find the orchestrator flow.
//  4. Dispatches a synthetic trigger into the orchestrator flow with
//     trigger_source='commitment', the commitment's description as the
//     content, and the original conversation_id so the response lands
//     in the channel where the promise was made.
//  5. On successful dispatch, PATCHes status → 'fulfilled'. On failure,
//     rolls back to 'pending' so the next poll retries.
//
// The poller is fire-and-forget per commitment: a failure on one
// commitment doesn't block the others. The status transition from
// 'pending' → 'firing' acts as a lightweight claim, serialising
// concurrent poller instances (multiple Launch replicas) via the
// single-writer guarantee of PATCH + the partial index on status.
//
// This is the service that turns "agent that remembers" into "agent
// that follows through". Without it, commitment rows accumulate in
// 'pending' status forever and the model's promises are never honoured.
package commitment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	log "github.com/sirupsen/logrus"
)

const (
	pollInterval = 30 * time.Second
	startupDelay = 10 * time.Second
	httpTimeout  = 15 * time.Second
)

// Service is the commitment poller loop.
type Service struct {
	config *config.Config
	db     *persistence.Service
	client *http.Client
}

// NewService creates and starts the commitment poller. The polling
// goroutine runs for the lifetime of the process. There is no Stop
// method — the goroutine dies when the process exits, which matches
// the pattern used by the schedule and S3 poll services.
func NewService(cfg *config.Config, db *persistence.Service) *Service {
	s := &Service{
		config: cfg,
		db:     db,
		client: &http.Client{Timeout: httpTimeout},
	}
	go s.watch()
	return s
}

func (s *Service) watch() {
	time.Sleep(startupDelay)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	log.Info("commitment poller started (30s interval)")

	for range ticker.C {
		s.poll()
	}
}

// commitment mirrors the JSON shape returned by GET /internal/commitment/due.
// We only need the fields relevant to dispatch — the full struct lives
// in the API types package which Launch doesn't import.
type commitment struct {
	ID             string          `json:"id"`
	AgentID        string          `json:"agent_id"`
	AgentUserID    *string         `json:"agent_user_id"`
	ConversationID *string         `json:"conversation_id"`
	Kind           string          `json:"kind"`
	Description    string          `json:"description"`
	Payload        json.RawMessage `json:"payload"`
	TriggerType    string          `json:"trigger_type"`
	MadeBy         string          `json:"made_by"`
}

func (s *Service) poll() {
	commitments := s.fetchDueCommitments()
	if len(commitments) == 0 {
		return
	}

	log.WithFields(log.Fields{
		"count": len(commitments),
	}).Debug("processing due commitments")

	for _, c := range commitments {
		s.processCommitment(c)
	}
}

func (s *Service) processCommitment(c commitment) {
	l := log.WithFields(log.Fields{
		"commitment_id": c.ID,
		"agent_id":      c.AgentID,
		"kind":          c.Kind,
	})

	// Step 1: claim by transitioning to 'firing'. If another poller
	// instance claimed it first (race on the PATCH), we'll get a
	// non-204 response and skip it.
	if err := s.updateCommitmentStatus(c.ID, "firing"); err != nil {
		l.WithError(err).Warn("failed to claim commitment, skipping")
		return
	}

	// Step 2: look up the agent's registration. If the agent is no
	// longer registered (stopped or deleted), expire the commitment
	// rather than retrying forever.
	reg, err := s.db.GetAgentRegistration(c.AgentID)
	if err != nil || reg == nil {
		l.Warn("agent not registered, expiring commitment")
		_ = s.updateCommitmentStatus(c.ID, "expired")
		return
	}

	if reg.OrchestratorFlowID == nil || *reg.OrchestratorFlowID == "" {
		l.Warn("agent has no orchestrator flow, expiring commitment")
		_ = s.updateCommitmentStatus(c.ID, "expired")
		return
	}

	// Step 3: dispatch a synthetic trigger into the agent's orchestrator
	// flow. The trigger data carries `trigger_source=commitment` so the
	// flow can branch on reactive vs proactive if it wants to (most
	// flows treat both identically — the AI just sees content and
	// responds). The commitment description becomes the "message" the
	// AI action sees.
	// Frame the content so the AI knows this is a scheduled follow-up,
	// not a new user message. The description is what the extraction
	// pipeline captured from the original promise.
	content := fmt.Sprintf("[SCHEDULED REMINDER] You previously promised to: %s. "+
		"Send a brief, friendly reminder to the user about this. "+
		"Do not discuss the conversation history in detail — just deliver the reminder naturally.", c.Description)

	triggerData := map[string]interface{}{
		"agent_id":       c.AgentID,
		"trigger_source": "commitment",
		"commitment_id":  c.ID,
		"content":        content,
		"sender":         "system",
	}
	if c.AgentUserID != nil {
		triggerData["agent_user_id"] = *c.AgentUserID
	}
	if c.ConversationID != nil {
		triggerData["conversation_id"] = *c.ConversationID

		// Look up the conversation to get channel details so the
		// orchestrator flow's send action can route the proactive
		// message back to the channel where the promise was made.
		if conv := s.fetchConversation(reg.APIURL, *c.ConversationID); conv != nil {
			triggerData["channel_type"] = conv.ChannelType
			triggerData["channel_id"] = conv.ChannelID
			if conv.ThreadID != "" {
				triggerData["thread_id"] = conv.ThreadID
			}
		}

		// Don't include conversation history for commitment wake-ups.
		// The [SCHEDULED REMINDER] content framing plus the commitment
		// description gives the AI everything it needs. Including history
		// causes the AI to address old conversation topics instead of
		// just delivering the reminder. History will be re-enabled once
		// outbound messages are properly stored in conversations.
	}
	// Default channel_type if conversation lookup didn't set it
	if _, has := triggerData["channel_type"]; !has {
		triggerData["channel_type"] = "commitment"
	}
	if len(c.Payload) > 0 && string(c.Payload) != "{}" && string(c.Payload) != "null" {
		triggerData["commitment_payload"] = c.Payload
	}

	// Build system prompt: persona + commitment-specific context.
	// The normal inbound path uses assembleSystemPrompt() which adds
	// Layer 0, memories, channel directives. The commitment poller
	// adds a simpler but sufficient version: persona + explicit
	// instruction that this is a scheduled follow-up the platform
	// is delivering.
	systemPrompt := ""
	if reg.SystemPrompt != nil {
		systemPrompt = *reg.SystemPrompt
	}
	systemPrompt += "\n\n━━━ Platform capabilities ━━━\n" +
		"You CAN set reminders and follow up proactively. The platform " +
		"delivers scheduled reminders automatically. When this execution " +
		"was triggered, it was because YOU previously promised to remind " +
		"the user. Deliver the reminder naturally as yourself — do not " +
		"break character, do not disclaim your abilities, do not mention " +
		"being an AI. You are fulfilling a promise you made.\n\n" +
		"━━━ Current time ━━━\n" + time.Now().Format("Monday, 2 January 2006 15:04 MST")
	triggerData["system_prompt"] = systemPrompt

	if err := s.dispatchOrchestrator(reg.APIURL, *reg.OrchestratorFlowID, reg.TriggerID, triggerData); err != nil {
		l.WithError(err).Warn("failed to dispatch commitment wake-up, rolling back to pending")
		_ = s.updateCommitmentStatus(c.ID, "pending")
		return
	}

	// Step 4: mark fulfilled. The flow has been dispatched — the
	// orchestrator will handle the AI reply and message delivery. The
	// commitment's job is done.
	if err := s.updateCommitmentStatus(c.ID, "fulfilled"); err != nil {
		l.WithError(err).Warn("failed to mark commitment fulfilled (dispatch already sent)")
	}

	l.Info("commitment fired and fulfilled")
}

// --- HTTP helpers ---

func (s *Service) fetchDueCommitments() []commitment {
	return s.fetchDueCommitmentsFrom(s.config.Automate.URL)
}

// fetchDueCommitmentsFrom is the testable core — takes the API base URL
// as a parameter so tests can point it at a httptest.Server.
func (s *Service) fetchDueCommitmentsFrom(apiBase string) []commitment {
	url := fmt.Sprintf("%s/api/v1/internal/commitment/due?limit=50", apiBase)
	resp, err := s.client.Get(url)
	if err != nil {
		log.WithError(err).Warn("failed to fetch due commitments")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result []commitment
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.WithError(err).Warn("failed to decode due commitments")
		return nil
	}
	return result
}

func (s *Service) updateCommitmentStatus(id, status string) error {
	return s.updateCommitmentStatusAt(s.config.Automate.URL, id, status)
}

func (s *Service) updateCommitmentStatusAt(apiBase, id, status string) error {
	body, _ := json.Marshal(map[string]string{"status": status})
	url := fmt.Sprintf("%s/api/v1/internal/commitment/%s", apiBase, id)

	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("status update returned %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

// conversationInfo is the subset of the conversation record the poller
// needs to reconstruct channel routing for proactive messages.
type conversationInfo struct {
	ChannelType string `json:"channel_type"`
	ChannelID   string `json:"channel_id"`
	ThreadID    string `json:"thread_id"`
}

func (s *Service) fetchConversation(apiURL, conversationID string) *conversationInfo {
	url := fmt.Sprintf("%s/api/v1/internal/conversation/%s", apiURL, conversationID)
	resp, err := s.client.Get(url)
	if err != nil {
		log.WithFields(log.Fields{
			"conversation_id": conversationID,
			"error":           err,
		}).Warn("failed to fetch conversation for commitment routing")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var conv conversationInfo
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		return nil
	}
	return &conv
}

func (s *Service) fetchConversationHistory(apiURL, conversationID string, limit int) []map[string]interface{} {
	url := fmt.Sprintf("%s/api/v1/internal/conversation/%s/history?limit=%d", apiURL, conversationID, limit)
	resp, err := s.client.Get(url)
	if err != nil {
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
	// direction=inbound/outbound, but the Anthropic/OpenAI actions
	// expect role=user/assistant.
	for _, msg := range messages {
		if dir, ok := msg["direction"].(string); ok {
			switch dir {
			case "inbound":
				msg["role"] = "user"
			case "outbound":
				msg["role"] = "assistant"
			default:
				msg["role"] = "user"
			}
		}
	}
	return messages
}

func (s *Service) dispatchOrchestrator(apiURL, flowID string, triggerID *string, data map[string]interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	var url string
	if triggerID != nil && *triggerID != "" {
		url = fmt.Sprintf("%s/api/v1/internal/flo/%s/trigger/%s/execute", apiURL, flowID, *triggerID)
	} else {
		url = fmt.Sprintf("%s/api/v1/internal/flo/%s/execute", apiURL, flowID)
	}

	resp, err := s.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("dispatch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("dispatch returned %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}
