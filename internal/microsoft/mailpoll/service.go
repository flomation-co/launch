// Package mailpoll polls Microsoft 365 mailboxes for new emails via the
// Microsoft Graph API and fires triggers when new messages arrive.
// Follows the same polling + state tracking pattern as the Google Drive
// and email trigger services.
package mailpoll

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/microsoft"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	DefaultPollInterval = 60 * time.Second
	LeaseDuration       = 2 * time.Minute
	sentinelKey         = "__initialized"
	graphAPI            = "https://graph.microsoft.com/v1.0"
)

// triggerConfig holds the configuration stored in trigger.Data.
type triggerConfig struct {
	FolderID  string `json:"folder_id"`
	Filter    string `json:"filter"`
	Account   string `json:"account"`
	Interval  string `json:"poll_interval"`
}

// emailMessage holds the fields extracted from a Graph API message response.
type emailMessage struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversationId"`
	Subject        string `json:"subject"`
	BodyPreview    string `json:"bodyPreview"`
	Body           struct {
		Content string `json:"content"`
	} `json:"body"`
	ReceivedDateTime string `json:"receivedDateTime"`
	HasAttachments   bool   `json:"hasAttachments"`
	Importance       string `json:"importance"`
	IsRead           bool   `json:"isRead"`
	From             struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
	ToRecipients []struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"toRecipients"`
}

// Service polls Microsoft 365 mailboxes for new emails.
type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	microsoft  *microsoft.Service
	instanceID string
}

// NewService creates a mail polling service and starts the background goroutine.
func NewService(cfg *config.Config, db *persistence.Service, triggerSvc *trigger.Service, msSvc *microsoft.Service) *Service {
	s := &Service{
		config:     cfg,
		db:         db,
		trigger:    triggerSvc,
		microsoft:  msSvc,
		instanceID: uuid.New().String(),
	}

	go s.watch()

	return s
}

func (s *Service) watch() {
	time.Sleep(10 * time.Second)

	for {
		s.poll()
		time.Sleep(DefaultPollInterval)
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeMicrosoftOutlook)
	if err != nil {
		log.WithError(err).Error("[ms-mail-poll] unable to get triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg triggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[ms-mail-poll] unable to parse trigger config")
		return
	}

	// Acquire lease to prevent duplicate polling across instances.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[ms-mail-poll] unable to acquire lease")
		return
	}
	if !acquired {
		return
	}

	// Resolve access token from trigger-scoped Microsoft account.
	accessToken, err := s.fetchToken(tr.ID, cfg.Account)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Warn("[ms-mail-poll] unable to fetch access token")
		return
	}

	// Build the messages endpoint.
	endpoint := s.buildMessagesEndpoint(cfg)

	// Fetch messages.
	messages, err := s.fetchMessages(accessToken, endpoint)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[ms-mail-poll] unable to fetch messages")
		return
	}

	// Load known state from DB.
	knownState, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[ms-mail-poll] unable to get trigger state")
		return
	}

	_, initialised := knownState[sentinelKey]
	isFirstPoll := !initialised

	// Mark as initialised on first poll.
	if isFirstPoll {
		if err := s.db.UpsertTriggerState(tr.ID, sentinelKey, []byte(`true`)); err != nil {
			log.WithError(err).Error("[ms-mail-poll] unable to set sentinel state")
		}
	}

	// Process messages — detect new ones.
	for _, msg := range messages {
		_, exists := knownState[msg.ID]

		// Record state for all messages.
		stateJSON, err := json.Marshal(msg.ReceivedDateTime)
		if err != nil {
			continue
		}
		if err := s.db.UpsertTriggerState(tr.ID, msg.ID, stateJSON); err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": tr.ID,
				"email_id":   msg.ID,
			}).Error("[ms-mail-poll] unable to upsert trigger state")
			continue
		}

		// Fire trigger for genuinely new messages (not first poll).
		if !exists && !isFirstPoll {
			s.fireTrigger(tr, msg, cfg.Account)
		}
	}
}

func (s *Service) buildMessagesEndpoint(cfg triggerConfig) string {
	var base string
	if cfg.FolderID != "" {
		base = fmt.Sprintf("%s/me/mailFolders/%s/messages", graphAPI, url.PathEscape(cfg.FolderID))
	} else {
		base = fmt.Sprintf("%s/me/messages", graphAPI)
	}

	params := url.Values{}
	params.Set("$top", "25")
	params.Set("$orderby", "receivedDateTime desc")
	params.Set("$select", "id,conversationId,subject,bodyPreview,body,receivedDateTime,hasAttachments,importance,isRead,from,toRecipients")

	if cfg.Filter != "" {
		params.Set("$filter", cfg.Filter)
	}

	return base + "?" + params.Encode()
}

func (s *Service) fetchMessages(accessToken, endpoint string) ([]emailMessage, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graph request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graph returned %d: %s", resp.StatusCode, truncate(body))
	}

	var result struct {
		Value []emailMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return result.Value, nil
}

func (s *Service) fetchToken(triggerID, accountFilter string) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/trigger/%s/resolve",
		s.config.Automate.URL, triggerID)

	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// Try the internal Microsoft token endpoint.
	tokenEndpoint := fmt.Sprintf("%s/api/v1/internal/trigger/%s/microsoft-tokens?purpose=mail_read",
		s.config.Automate.URL, triggerID)

	tokenReq, err := http.NewRequest(http.MethodGet, tokenEndpoint, nil)
	if err != nil {
		return "", err
	}

	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = tokenResp.Body.Close() }()

	if tokenResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", tokenResp.StatusCode)
	}

	var tokens []struct {
		Email       string `json:"email"`
		Label       string `json:"label"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokens); err != nil {
		return "", err
	}

	// Filter by account if specified.
	for _, t := range tokens {
		if t.Error != "" {
			continue
		}
		if accountFilter != "" {
			if !strings.EqualFold(t.Email, accountFilter) &&
				!strings.EqualFold(t.Label, accountFilter) {
				continue
			}
		}
		return t.AccessToken, nil
	}

	return "", fmt.Errorf("no active Microsoft account with mail_read permissions")
}

func (s *Service) fireTrigger(tr *launch.Trigger, msg emailMessage, account string) {
	// Build recipient list.
	var toAddresses []string
	for _, r := range msg.ToRecipients {
		toAddresses = append(toAddresses, r.EmailAddress.Address)
	}

	data := map[string]interface{}{
		"email_id":        msg.ID,
		"conversation_id": msg.ConversationID,
		"from":            msg.From.EmailAddress.Address,
		"to":              strings.Join(toAddresses, ", "),
		"subject":         msg.Subject,
		"body_preview":    msg.BodyPreview,
		"body":            msg.Body.Content,
		"received_at":     msg.ReceivedDateTime,
		"has_attachments":  msg.HasAttachments,
		"importance":      msg.Importance,
		"is_read":         msg.IsRead,
		"account":         account,
		"triggered_at":    time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"email_id":   msg.ID,
		}).Error("[ms-mail-poll] unable to fire trigger")
	} else {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"email_id":   msg.ID,
			"subject":    msg.Subject,
		}).Info("[ms-mail-poll] fired trigger for new email")
	}
}

func truncate(b []byte) string {
	if len(b) > 512 {
		return string(b[:512])
	}
	return string(b)
}
