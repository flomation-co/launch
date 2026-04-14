// Package pendingaction implements the Phase 5 pending action poller —
// the proactive notification mechanism that dispatches confirmation
// prompts to users when identity_link (or other) pending actions are
// created by the extraction pipeline.
//
// Without this poller, pending actions sit in 'awaiting_confirmation'
// until the user happens to send another message, at which point the
// system prompt includes the confirmation instruction. This poller
// removes that dependency on user-initiated turns by dispatching a
// synthetic trigger into the agent's orchestrator flow.
//
// Every 15 seconds, the poller:
//  1. Fetches unnotified pending actions (notified_at IS NULL,
//     status='awaiting_confirmation') from the API.
//  2. For each, looks up the agent registration and the conversation
//     where the action was created.
//  3. Dispatches a synthetic trigger with trigger_source='pending_action'
//     and content framed as a confirmation request.
//  4. Stamps notified_at via PATCH so the action isn't re-dispatched.
package pendingaction

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
	pollInterval = 15 * time.Second
	startupDelay = 12 * time.Second
	httpTimeout  = 15 * time.Second
)

// Service is the pending action poller loop.
type Service struct {
	config *config.Config
	db     *persistence.Service
	client *http.Client
}

// NewService creates and starts the pending action poller.
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

	log.Info("pending action poller started (15s interval)")

	for range ticker.C {
		s.poll()
	}
}

// pendingAction mirrors the JSON shape returned by the API.
type pendingAction struct {
	ID                 string          `json:"id"`
	AgentID            string          `json:"agent_id"`
	AgentUserID        string          `json:"agent_user_id"`
	Type               string          `json:"type"`
	Payload            json.RawMessage `json:"payload"`
	Evidence           string          `json:"evidence"`
	Status             string          `json:"status"`
	SourceConversation *string         `json:"source_conversation"`
}

func (s *Service) poll() {
	actions := s.fetchUnnotified()
	if len(actions) == 0 {
		return
	}

	log.WithField("count", len(actions)).Debug("processing unnotified pending actions")

	for _, pa := range actions {
		s.processAction(pa)
	}
}

// pollerActionTypes is the set of pending action types the poller should
// dispatch proactive messages for. Other types (correct_memory, forget_memory)
// are handled internally by the extraction pipeline and should not trigger
// user-facing messages.
var pollerActionTypes = map[string]bool{
	"identity_link":              true,
	"identity_link_verification": true,
}

func (s *Service) processAction(pa pendingAction) {
	l := log.WithFields(log.Fields{
		"pending_action_id": pa.ID,
		"agent_id":          pa.AgentID,
		"type":              pa.Type,
	})

	// Only dispatch user-facing messages for identity linking types.
	// Other types are handled internally and should not trigger messages.
	if !pollerActionTypes[pa.Type] {
		// Mark as notified so we don't re-check it every cycle.
		_ = s.markNotified(pa.ID)
		l.Debug("skipping non-identity pending action type")
		return
	}

	// Look up the agent registration.
	reg, err := s.db.GetAgentRegistration(pa.AgentID)
	if err != nil || reg == nil {
		l.Warn("agent not registered, skipping pending action notification")
		return
	}
	if reg.OrchestratorFlowID == nil || *reg.OrchestratorFlowID == "" {
		l.Warn("agent has no orchestrator flow, skipping")
		return
	}

	// Build the confirmation prompt content based on type.
	var content string
	switch pa.Type {
	case "identity_link":
		content = fmt.Sprintf(
			"[SYSTEM: IDENTITY LINK REQUEST] The user previously said: %q. "+
				"They mentioned an identity on another messaging channel. "+
				"Ask them to CONFIRM that this is them so you can link their conversations. "+
				"This is NOT about connecting Google accounts or OAuth — this is about "+
				"recognising them as the same person when they message you from different "+
				"channels (e.g. Telegram vs Email). Say something like: "+
				"\"You mentioned you're also reachable at [identity] — shall I link that "+
				"so I recognise you as the same person across channels? Just say yes to confirm.\"",
			pa.Evidence)
	case "identity_link_verification":
		// Extract source channel from payload for context.
		var payload map[string]interface{}
		_ = json.Unmarshal(pa.Payload, &payload)
		sourceChannel, _ := payload["source_channel"].(string)
		if sourceChannel == "" {
			sourceChannel = "another channel"
		}

		content = fmt.Sprintf(
			"[SYSTEM: IDENTITY VERIFICATION — SEND A NEW MESSAGE] "+
				"You need to verify this user's identity. They were linked from %s. "+
				"Send them a natural, friendly message — NOT a robotic verification. "+
				"For email: use email_send (NOT email_reply). "+
				"Write something like: \"Hey, just checking — is this the right email for you? "+
				"Someone on %s mentioned this address. Just reply yes if that's you.\" "+
				"Keep your own voice and personality. Don't mention 'identity verification' "+
				"or 'claims to be you' — just confirm it's them naturally.",
			sourceChannel, sourceChannel)
	default:
		content = fmt.Sprintf(
			"[SYSTEM: PENDING CONFIRMATION] A %s needs confirmation. Context: %q. "+
				"Ask the user to confirm or decline.",
			pa.Type, pa.Evidence)
	}

	// Build trigger data.
	triggerData := map[string]interface{}{
		"agent_id":          pa.AgentID,
		"agent_user_id":     pa.AgentUserID,
		"trigger_source":    "pending_action",
		"pending_action_id": pa.ID,
		"content":           content,
		"sender":            "system",
	}

	// For identity_link_verification, route to the target user's channel
	// rather than the source conversation (which may not exist or may be
	// on the wrong channel). Look up the target identity to get channel info.
	if pa.Type == "identity_link_verification" {
		identity := s.lookupUserIdentity(reg.APIURL, pa.AgentID, pa.AgentUserID)
		if identity != nil {
			triggerData["channel_type"] = identity.ChannelType
			triggerData["channel_external_id"] = identity.ChannelExternalID
			// Set recipient info so the AI knows who to contact.
			triggerData["recipient"] = identity.ChannelExternalID
			// Set channel-specific metadata so deriveExternalID works
			switch identity.ChannelType {
			case "email":
				triggerData["from"] = identity.ChannelExternalID
				// Override content for email: include recipient and natural tone
				content = fmt.Sprintf(
					"[SYSTEM: SEND VERIFICATION EMAIL to %s] "+
						"Use email_send (NOT email_reply). Recipient: %s. "+
						"Subject: something casual like \"Quick check\" or \"Is this you?\". "+
						"Body: Write a natural, friendly message checking this is the right email. "+
						"Something like: \"Hey, just wanted to check this is the right email for you — "+
						"I've got you on another channel and want to make sure I recognise you across both. "+
						"Just reply yes if that's you!\" Keep your personality. Don't be robotic.",
					identity.ChannelExternalID, identity.ChannelExternalID)
				triggerData["content"] = content
			case "telegram":
				triggerData["sender_id"] = identity.ChannelExternalID
			case "slack":
				triggerData["user_id"] = identity.ChannelExternalID
			}
		}
	} else {
		// Route to the conversation where the action was created.
		if pa.SourceConversation != nil && *pa.SourceConversation != "" {
			triggerData["conversation_id"] = *pa.SourceConversation

			if conv := s.fetchConversation(reg.APIURL, *pa.SourceConversation); conv != nil {
				triggerData["channel_type"] = conv.ChannelType
				triggerData["channel_id"] = conv.ChannelID
				if conv.ThreadID != "" {
					triggerData["thread_id"] = conv.ThreadID
				}
			}
		}
	}

	// Default channel_type if conversation lookup didn't set it.
	if _, has := triggerData["channel_type"]; !has {
		triggerData["channel_type"] = "system"
	}

	// Build system prompt with persona + pending action context.
	systemPrompt := ""
	if reg.SystemPrompt != nil {
		systemPrompt = *reg.SystemPrompt
	}

	if pa.Type == "identity_link_verification" {
		systemPrompt += "\n\n━━━ URGENT TASK ━━━\n" +
			"You are sending a verification message to a user on a different channel. " +
			"Someone on another messaging platform claims to also be this user. " +
			"Your ONLY job right now is to send them a message asking to confirm. " +
			"For EMAIL: use the email_send tool (NOT email_reply). The recipient address is in the message content. " +
			"For TELEGRAM: send a regular message. For SLACK: send a regular message. " +
			"Do NOT ask for more context. Do NOT say you don't have records. " +
			"Just send the verification message as instructed in the content.\n"
	} else {
		systemPrompt += "\n\n━━━ ACTION REQUIRED ━━━\n" +
			"CRITICAL: You MUST address the pending confirmation described in the message. " +
			"This is about IDENTITY LINKING — recognising the user as the same person across " +
			"different messaging channels (Telegram, Slack, Email, etc). It is NOT about " +
			"Google OAuth, calendar connections, or account settings. " +
			"Ask the user to confirm with a simple yes/no. Do NOT offer OAuth links or connection options.\n"
	}
	triggerData["system_prompt"] = systemPrompt

	// Include conversation history for context.
	if pa.SourceConversation != nil && *pa.SourceConversation != "" {
		if history := s.fetchConversationHistory(reg.APIURL, *pa.SourceConversation, 5); history != nil {
			triggerData["conversation_history"] = history
		}
	}

	// Dispatch.
	if err := s.dispatchOrchestrator(reg.APIURL, *reg.OrchestratorFlowID, reg.TriggerID, triggerData); err != nil {
		l.WithError(err).Warn("failed to dispatch pending action confirmation")
		return
	}

	// Mark as notified so we don't re-fire.
	if err := s.markNotified(pa.ID); err != nil {
		l.WithError(err).Warn("failed to mark pending action as notified (dispatch already sent)")
	}

	l.Info("pending action confirmation dispatched")
}

// identityInfo is the subset of agent_identity the poller needs.
type identityInfo struct {
	ChannelType       string `json:"channel_type"`
	ChannelExternalID string `json:"channel_external_id"`
}

// lookupUserIdentity fetches the first identity for a given agent user.
func (s *Service) lookupUserIdentity(apiURL, agentID, agentUserID string) *identityInfo {
	// Use the pending-action list endpoint with user scoping to find
	// the user's identities. Actually, we need a direct identity lookup.
	// Use the agent's user identities list.
	url := fmt.Sprintf("%s/api/v1/internal/agent/%s/identity?agent_user_id=%s",
		apiURL, agentID, agentUserID)
	resp, err := s.client.Get(url)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_user_id": agentUserID,
			"error":         err,
		}).Warn("failed to lookup user identity for verification routing")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var identities []identityInfo
	if err := json.NewDecoder(resp.Body).Decode(&identities); err != nil || len(identities) == 0 {
		return nil
	}
	return &identities[0]
}

// --- HTTP helpers ---

func (s *Service) fetchUnnotified() []pendingAction {
	url := fmt.Sprintf("%s/api/v1/internal/pending-action/unnotified", s.config.Automate.URL)
	resp, err := s.client.Get(url)
	if err != nil {
		log.WithError(err).Warn("failed to fetch unnotified pending actions")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result []pendingAction
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.WithError(err).Warn("failed to decode unnotified pending actions")
		return nil
	}
	return result
}

func (s *Service) markNotified(id string) error {
	url := fmt.Sprintf("%s/api/v1/internal/pending-action/%s/notified", s.config.Automate.URL, id)
	req, err := http.NewRequest(http.MethodPatch, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("mark notified returned %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

type conversationInfo struct {
	ChannelType string `json:"channel_type"`
	ChannelID   string `json:"channel_id"`
	ThreadID    string `json:"thread_id"`
}

func (s *Service) fetchConversation(apiURL, conversationID string) *conversationInfo {
	url := fmt.Sprintf("%s/api/v1/internal/conversation/%s", apiURL, conversationID)
	resp, err := s.client.Get(url)
	if err != nil {
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