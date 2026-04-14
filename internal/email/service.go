// Package email provides a polling service that monitors Gmail accounts
// for new messages and fires triggers when emails arrive. Uses Gmail's
// History API for efficient incremental polling — only new messages
// since the last known historyId are fetched.
package email

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/agent"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	DefaultPollInterval = 60 * time.Second
	MinPollInterval     = 30 * time.Second
	LeaseDuration       = 2 * time.Minute

	gmailAPIBase = "https://gmail.googleapis.com/gmail/v1/users/me"
	maxBodyBytes = 10 * 1024 // 10KB body truncation
)

type emailTriggerConfig struct {
	GmailQuery   string `json:"gmail_query"`
	PollInterval string `json:"poll_interval"`
	Account      string `json:"account"`
}

type tokenInfo struct {
	Email       string `json:"email"`
	Label       string `json:"label"`
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

// historyState is stored in trigger_state per (trigger_id, account_email).
type historyState struct {
	HistoryID string `json:"history_id"`
}

type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	agent      *agent.Service
	instanceID string
}

func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service, agentSvc *agent.Service) *Service {
	s := &Service{
		config:     config,
		db:         db,
		trigger:    trigger,
		agent:      agentSvc,
		instanceID: uuid.New().String(),
	}

	go s.watch()

	return s
}

func (s *Service) watch() {
	time.Sleep(10 * time.Second) // Let other services start first

	for {
		s.poll()
		time.Sleep(DefaultPollInterval)
	}
}

func (s *Service) poll() {
	// Path 1: standalone flow email triggers
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeEmail)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("unable to get email triggers")
	} else {
		for _, tr := range triggers {
			if tr.DisabledAt != nil {
				continue
			}
			s.checkTrigger(tr)
		}
	}

	// Path 2: agent email channels — dispatch as inbound agent messages
	if s.agent != nil {
		for _, info := range s.agent.GetAgentsWithEmailChannel() {
			s.checkAgentEmail(info.AgentID)
		}
	}
}

// checkAgentEmail polls for new emails on an agent's email channel
// and dispatches them as inbound messages through the agent pipeline.
func (s *Service) checkAgentEmail(agentID string) {
	// Fetch tokens using the agent ID as the trigger_google_account scope
	endpoint := fmt.Sprintf("%s/api/v1/internal/trigger/%s/google-refresh-tokens?purpose=email_read",
		s.config.Automate.URL, agentID)
	tokens := s.fetchAndRefreshTokens(endpoint)

	if len(tokens) == 0 {
		return
	}

	for _, token := range tokens {
		if token.Error != "" || token.AccessToken == "" {
			continue
		}
		s.checkAgentAccount(agentID, token)
	}
}

func (s *Service) checkAgentAccount(agentID string, token tokenInfo) {
	stateKey := "gmail_agent_" + agentID + "_" + token.Email

	// Get last known historyId from email_poll_state (not trigger_state,
	// since agent IDs aren't valid trigger foreign keys)
	var state historyState
	if raw, err := s.db.GetEmailPollState(agentID, stateKey); err == nil && raw != nil {
		_ = json.Unmarshal(raw, &state)
	}

	if state.HistoryID == "" {
		// First run: baseline
		historyID, err := getProfileHistoryID(token.AccessToken)
		if err != nil {
			return
		}
		state.HistoryID = historyID
		data, _ := json.Marshal(state)
		_ = s.db.UpsertEmailPollState(agentID, stateKey, data)
		log.WithFields(log.Fields{
			"agent_id":   agentID,
			"account":    token.Email,
			"history_id": historyID,
		}).Info("agent email channel initialised")
		return
	}

	// Incremental poll
	newMessages, newHistoryID, err := getHistory(token.AccessToken, state.HistoryID, "")
	if err != nil {
		return
	}

	if newHistoryID != "" && newHistoryID != state.HistoryID {
		state.HistoryID = newHistoryID
		data, _ := json.Marshal(state)
		_ = s.db.UpsertEmailPollState(agentID, stateKey, data)
	}

	// Dispatch each new email as an inbound agent message.
	// Skip emails sent FROM the agent's own account to prevent infinite loops
	// (agent sends reply → Gmail adds to inbox → poller picks up → agent replies → ...)
	for _, msg := range newMessages {
		fromEmail := extractEmailAddr(msg.From)
		if strings.EqualFold(fromEmail, token.Email) {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"email_id": msg.ID,
				"from":     msg.From,
			}).Debug("skipping email from agent's own account")
			continue
		}

		content := fmt.Sprintf("New email from %s\nSubject: %s\n\n%s", msg.From, msg.Subject, msg.BodyText)
		if len(content) > 4000 {
			content = content[:4000] + "\n... [truncated]"
		}

		err := s.agent.HandleInboundMessage(agentID, agent.InboundMessage{
			ChannelType: "email",
			Sender:      msg.From,
			Content:     content,
			Metadata: map[string]interface{}{
				"email_id":        msg.ID,
				"thread_id":       msg.ThreadID,
				"from":            msg.From,
				"to":              msg.To,
				"subject":         msg.Subject,
				"date":            msg.Date,
				"labels":          msg.Labels,
				"has_attachments": msg.HasAttachments,
				"account":         token.Email,
				"channel_id":      msg.ID,
				"user_id":         msg.From,
				"user_name":       msg.From,
			},
		})
		if err != nil {
			log.WithFields(log.Fields{
				"error":    err,
				"agent_id": agentID,
				"email_id": msg.ID,
			}).Warn("failed to dispatch email to agent")
		} else {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"from":     msg.From,
				"subject":  msg.Subject,
			}).Info("email dispatched to agent")
		}
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg emailTriggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("unable to parse email trigger config")
		return
	}

	// Acquire lease
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil || !acquired {
		return
	}

	// The trigger is linked to a flow which is owned by a user. We need
	// the agent_user_id to fetch tokens. For email triggers, this is
	// stored in the trigger data alongside the gmail_query.
	// For now, we resolve tokens by getting ALL email_read tokens from
	// the Launch→API chain and filtering by account if specified.
	tokens, err := s.fetchTokens(tr, cfg.Account)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Warn("unable to fetch email tokens for trigger")
		return
	}

	for _, token := range tokens {
		if token.Error != "" || token.AccessToken == "" {
			continue
		}
		if cfg.Account != "" &&
			!strings.EqualFold(token.Email, cfg.Account) &&
			!strings.EqualFold(token.Label, cfg.Account) {
			continue
		}
		s.checkAccount(tr, token, cfg.GmailQuery)
	}
}

func (s *Service) checkAccount(tr *launch.Trigger, token tokenInfo, query string) {
	stateKey := "gmail_" + token.Email

	// Get the last known historyId for this account+trigger
	var state historyState
	allState, err := s.db.GetTriggerState(tr.ID)
	if err == nil && allState != nil {
		if raw, ok := allState[stateKey]; ok {
			_ = json.Unmarshal(raw, &state)
		}
	}

	if state.HistoryID == "" {
		// First run: get the current historyId as baseline (don't fire on existing emails)
		historyID, err := getProfileHistoryID(token.AccessToken)
		if err != nil {
			log.WithFields(log.Fields{
				"error":   err,
				"account": token.Email,
			}).Warn("unable to get Gmail profile for initial historyId")
			return
		}
		state.HistoryID = historyID
		data, _ := json.Marshal(state)
		_ = s.db.UpsertTriggerState(tr.ID, stateKey, data)
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"account":    token.Email,
			"history_id": historyID,
		}).Info("email trigger initialised with baseline historyId")
		return
	}

	// Incremental poll: get new messages since last historyId
	newMessages, newHistoryID, err := getHistory(token.AccessToken, state.HistoryID, query)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"account":    token.Email,
		}).Warn("unable to poll Gmail history")
		return
	}

	// Update historyId even if no new messages
	if newHistoryID != "" && newHistoryID != state.HistoryID {
		state.HistoryID = newHistoryID
		data, _ := json.Marshal(state)
		_ = s.db.UpsertTriggerState(tr.ID, stateKey, data)
	}

	// Fire trigger for each new message, skipping emails from our own account
	for _, msg := range newMessages {
		fromEmail := extractEmailAddr(msg.From)
		if strings.EqualFold(fromEmail, token.Email) {
			continue
		}
		triggerData := map[string]interface{}{
			"email_id":        msg.ID,
			"thread_id":       msg.ThreadID,
			"from":            msg.From,
			"to":              msg.To,
			"subject":         msg.Subject,
			"snippet":         msg.Snippet,
			"body_text":       msg.BodyText,
			"date":            msg.Date,
			"labels":          msg.Labels,
			"has_attachments": msg.HasAttachments,
			"account":         token.Email,
			"triggered_at":    time.Now().UTC().Format(time.RFC3339),
		}

		if err := s.trigger.Trigger(tr, triggerData); err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": tr.ID,
				"email_id":   msg.ID,
			}).Error("unable to fire email trigger")
		} else {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"email_id":   msg.ID,
				"from":       msg.From,
				"subject":    msg.Subject,
			}).Info("email trigger fired")
		}
	}
}

// --- Token fetching ---

func (s *Service) fetchTokens(tr *launch.Trigger, accountFilter string) ([]tokenInfo, error) {
	// Try two token sources in order:
	// 1. Trigger-scoped accounts (configured via "Add Account" in the editor)
	// 2. Agent-user accounts (injected from agent context at runtime)
	//
	// For standalone flows, only (1) is available.
	// For agent flows, (2) takes priority but (1) serves as fallback.

	// Source 1: trigger-scoped tokens (refresh in-process since we have
	// the Google credentials locally — no need to proxy through ourselves)
	triggerEndpoint := fmt.Sprintf("%s/api/v1/internal/trigger/%s/google-refresh-tokens?purpose=email_read",
		s.config.Automate.URL, tr.ID)
	triggerTokens := s.fetchAndRefreshTokens(triggerEndpoint)

	// Source 2: agent-user tokens (if agent_user_id is in trigger data)
	var agentTokens []tokenInfo
	var triggerData struct {
		AgentUserID string `json:"agent_user_id"`
	}
	_ = json.Unmarshal(tr.Data, &triggerData)
	if triggerData.AgentUserID != "" {
		agentEndpoint := fmt.Sprintf("%s/api/v1/internal/agent-user/%s/google-refresh-tokens?purpose=email_read",
			s.config.Automate.URL, triggerData.AgentUserID)
		agentTokens = s.fetchAndRefreshTokens(agentEndpoint)
	}

	// Merge: trigger tokens first, then agent tokens (dedup by email)
	seen := make(map[string]bool)
	var all []tokenInfo
	for _, t := range triggerTokens {
		seen[t.Email] = true
		all = append(all, t)
	}
	for _, t := range agentTokens {
		if !seen[t.Email] {
			all = append(all, t)
		}
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no email_read tokens available (trigger or agent-user)")
	}

	return all, nil
}

// fetchAndRefreshTokens fetches raw refresh tokens from the API and
// exchanges each for an access token using Launch's Google credentials.
func (s *Service) fetchAndRefreshTokens(endpoint string) []tokenInfo {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	type rawAccount struct {
		Email        string `json:"email"`
		Label        string `json:"label"`
		RefreshToken string `json:"refresh_token"`
	}
	var accounts []rawAccount
	if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil {
		return nil
	}

	if s.config.Google == nil {
		return nil
	}

	var tokens []tokenInfo
	for _, acct := range accounts {
		if acct.RefreshToken == "" {
			continue
		}
		accessToken, err := refreshGoogleToken(
			acct.RefreshToken,
			s.config.Google.ClientID,
			s.config.Google.ClientSecret,
		)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
				"email": acct.Email,
			}).Warn("failed to refresh Google token for email trigger")
			continue
		}
		tokens = append(tokens, tokenInfo{
			Email:       acct.Email,
			Label:       acct.Label,
			AccessToken: accessToken,
		})
	}
	return tokens
}

// --- Gmail API calls ---

type emailMessage struct {
	ID             string `json:"id"`
	ThreadID       string `json:"thread_id"`
	From           string `json:"from"`
	To             string `json:"to"`
	Subject        string `json:"subject"`
	Snippet        string `json:"snippet"`
	BodyText       string `json:"body_text"`
	Date           string `json:"date"`
	Labels         string `json:"labels"`
	HasAttachments bool   `json:"has_attachments"`
}

func getProfileHistoryID(accessToken string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, gmailAPIBase+"/profile", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var profile struct {
		HistoryID string `json:"historyId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return "", err
	}
	return profile.HistoryID, nil
}

func getHistory(accessToken, startHistoryID, query string) ([]emailMessage, string, error) {
	endpoint := fmt.Sprintf("%s/history?startHistoryId=%s&historyTypes=messageAdded",
		gmailAPIBase, startHistoryID)

	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// historyId too old — get a fresh baseline
		newID, err := getProfileHistoryID(accessToken)
		return nil, newID, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("gmail history API returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		History []struct {
			MessagesAdded []struct {
				Message struct {
					ID       string   `json:"id"`
					ThreadID string   `json:"threadId"`
					LabelIDs []string `json:"labelIds"`
				} `json:"message"`
			} `json:"messagesAdded"`
		} `json:"history"`
		HistoryID string `json:"historyId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", err
	}

	// Deduplicate message IDs
	seen := make(map[string]bool)
	var messageIDs []string
	for _, h := range result.History {
		for _, ma := range h.MessagesAdded {
			// Only trigger on INBOX messages (not sent, draft, etc.)
			hasInbox := false
			for _, l := range ma.Message.LabelIDs {
				if l == "INBOX" {
					hasInbox = true
					break
				}
			}
			if !hasInbox {
				continue
			}
			if !seen[ma.Message.ID] {
				seen[ma.Message.ID] = true
				messageIDs = append(messageIDs, ma.Message.ID)
			}
		}
	}

	// Fetch full details for each new message
	var messages []emailMessage
	for _, msgID := range messageIDs {
		msg, err := fetchMessage(accessToken, msgID)
		if err != nil {
			continue
		}
		// Apply query filter if specified (client-side, since History API
		// doesn't support query filtering)
		if query != "" && !matchesQuery(msg, query) {
			continue
		}
		messages = append(messages, *msg)
	}

	return messages, result.HistoryID, nil
}

func fetchMessage(accessToken, messageID string) (*emailMessage, error) {
	endpoint := fmt.Sprintf("%s/messages/%s?format=full", gmailAPIBase, messageID)
	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var raw struct {
		ID       string   `json:"id"`
		ThreadID string   `json:"threadId"`
		LabelIDs []string `json:"labelIds"`
		Snippet  string   `json:"snippet"`
		Payload  struct {
			MimeType string `json:"mimeType"`
			Headers  []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
			Body struct {
				Data string `json:"data"`
			} `json:"body"`
			Parts []mimePart `json:"parts"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	msg := &emailMessage{
		ID:       raw.ID,
		ThreadID: raw.ThreadID,
		Snippet:  raw.Snippet,
		Labels:   strings.Join(raw.LabelIDs, ", "),
	}

	for _, h := range raw.Payload.Headers {
		switch h.Name {
		case "From":
			msg.From = h.Value
		case "To":
			msg.To = h.Value
		case "Subject":
			msg.Subject = h.Value
		case "Date":
			msg.Date = h.Value
		}
	}

	// Check for attachments
	for _, part := range raw.Payload.Parts {
		if part.Filename != "" {
			msg.HasAttachments = true
			break
		}
	}

	// Extract body
	plainText, htmlText := extractBody(raw.Payload.MimeType, raw.Payload.Body.Data, raw.Payload.Parts)
	if plainText != "" {
		msg.BodyText = plainText
	} else if htmlText != "" {
		msg.BodyText = stripHTML(htmlText)
	}
	if len(msg.BodyText) > maxBodyBytes {
		msg.BodyText = msg.BodyText[:maxBodyBytes]
	}

	return msg, nil
}

// matchesQuery does a basic client-side filter. For simple queries like
// "from:user@example.com" or "subject:urgent", this checks the relevant
// fields. For complex queries, all messages pass through (the user can
// refine in the flow).
func matchesQuery(msg *emailMessage, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))

	// from: filter
	if strings.HasPrefix(q, "from:") {
		return strings.Contains(strings.ToLower(msg.From), strings.TrimPrefix(q, "from:"))
	}
	// subject: filter
	if strings.HasPrefix(q, "subject:") {
		return strings.Contains(strings.ToLower(msg.Subject), strings.TrimPrefix(q, "subject:"))
	}
	// is:unread — check labels
	if q == "is:unread" {
		return strings.Contains(msg.Labels, "UNREAD")
	}

	// Generic: check if query appears anywhere
	lower := strings.ToLower(msg.From + " " + msg.Subject + " " + msg.Snippet)
	return strings.Contains(lower, q)
}

// --- MIME helpers ---

type mimePart struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []mimePart `json:"parts"`
}

func extractBody(mimeType, bodyData string, parts []mimePart) (string, string) {
	if bodyData != "" {
		decoded := decodeBase64URL(bodyData)
		if strings.HasPrefix(mimeType, "text/plain") {
			return decoded, ""
		}
		if strings.HasPrefix(mimeType, "text/html") {
			return "", decoded
		}
	}
	var plainText, htmlText string
	for _, part := range parts {
		pt, ht := extractBody(part.MimeType, part.Body.Data, part.Parts)
		if pt != "" && plainText == "" {
			plainText = pt
		}
		if ht != "" && htmlText == "" {
			htmlText = ht
		}
	}
	return plainText, htmlText
}

func decodeBase64URL(s string) string {
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s
	}
	return string(decoded)
}

var htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

func stripHTML(html string) string {
	text := htmlTagRegex.ReplaceAllString(html, "")
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// extractEmailAddr pulls the bare email from "Name <email>" format.
func extractEmailAddr(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "<"); idx != -1 {
		end := strings.Index(s, ">")
		if end > idx {
			return strings.TrimSpace(s[idx+1 : end])
		}
	}
	return s
}

const googleTokenURL = "https://oauth2.googleapis.com/token" // #nosec G101 — not a credential, it's a public Google endpoint

func refreshGoogleToken(refreshToken, clientID, clientSecret string) (string, error) {
	data := url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(googleTokenURL, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response")
	}
	return result.AccessToken, nil
}
