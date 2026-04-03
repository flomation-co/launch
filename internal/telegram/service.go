// Package telegram handles Telegram Bot API webhook registration and update parsing.
// When an agent with a Telegram channel is started, the service registers a webhook
// with Telegram pointing to this Launch instance. Incoming updates are parsed and
// routed to the agent dispatch pipeline.
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	telegramAPIBase = "https://api.telegram.org"
	httpTimeout     = 15 * time.Second
)

// Service manages Telegram webhook registrations for agents.
type Service struct {
	mu          sync.RWMutex
	webhookBase string // e.g. "https://launch.flomation.app"
	registered  map[string]string // agentID → bot token (for cleanup)
	client      *http.Client
}

// NewService creates a Telegram service. webhookBase is the public URL
// of the Launch service (e.g. "https://launch.flomation.app").
func NewService(webhookBase string) *Service {
	return &Service{
		webhookBase: webhookBase,
		registered:  make(map[string]string),
		client:      &http.Client{Timeout: httpTimeout},
	}
}

// RegisterWebhook calls the Telegram Bot API to set the webhook URL for an agent.
// The webhook points to: {webhookBase}/webhook/telegram/{agentID}
func (s *Service) RegisterWebhook(agentID string, botToken string) error {
	webhookURL := fmt.Sprintf("%s/webhook/telegram/%s", s.webhookBase, agentID)

	payload, err := json.Marshal(map[string]interface{}{
		"url":             webhookURL,
		"allowed_updates": []string{"message"},
		"drop_pending_updates": true,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal setWebhook payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/setWebhook", telegramAPIBase, botToken)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to call setWebhook: %w", err)
	}
	defer resp.Body.Close()

	var result telegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode setWebhook response: %w", err)
	}

	if !result.OK {
		return fmt.Errorf("setWebhook failed: %s (code %d)", result.Description, result.ErrorCode)
	}

	s.mu.Lock()
	s.registered[agentID] = botToken
	s.mu.Unlock()

	log.WithFields(log.Fields{
		"agent_id":    agentID,
		"webhook_url": webhookURL,
	}).Info("telegram webhook registered")

	return nil
}

// DeregisterWebhook removes the Telegram webhook for an agent.
func (s *Service) DeregisterWebhook(agentID string) error {
	s.mu.RLock()
	botToken, exists := s.registered[agentID]
	s.mu.RUnlock()

	if !exists {
		return nil // nothing to deregister
	}

	url := fmt.Sprintf("%s/bot%s/deleteWebhook", telegramAPIBase, botToken)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("failed to call deleteWebhook: %w", err)
	}
	defer resp.Body.Close()

	s.mu.Lock()
	delete(s.registered, agentID)
	s.mu.Unlock()

	log.WithFields(log.Fields{
		"agent_id": agentID,
	}).Info("telegram webhook deregistered")

	return nil
}

// ParseUpdate extracts message information from a Telegram webhook update.
// Returns nil if the update doesn't contain a usable message.
func ParseUpdate(body []byte) *ParsedMessage {
	var update telegramUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		return nil
	}

	msg := update.Message
	if msg == nil {
		// Could be an edited message or other update type
		msg = update.EditedMessage
	}
	if msg == nil {
		return nil
	}

	parsed := &ParsedMessage{
		MessageID: msg.MessageID,
		ChatID:    msg.Chat.ID,
		Text:      msg.Text,
		Date:      time.Unix(msg.Date, 0),
	}

	// Build sender name
	if msg.From != nil {
		parsed.SenderID = msg.From.ID
		parsed.SenderUsername = msg.From.Username
		if msg.From.FirstName != "" {
			parsed.SenderName = msg.From.FirstName
			if msg.From.LastName != "" {
				parsed.SenderName += " " + msg.From.LastName
			}
		}
	}

	// Chat metadata
	parsed.ChatType = msg.Chat.Type
	if msg.Chat.Title != "" {
		parsed.ChatTitle = msg.Chat.Title
	}

	return parsed
}

// ParsedMessage contains the extracted fields from a Telegram update.
type ParsedMessage struct {
	MessageID      int64     `json:"message_id"`
	ChatID         int64     `json:"chat_id"`
	ChatType       string    `json:"chat_type"` // private, group, supergroup, channel
	ChatTitle      string    `json:"chat_title,omitempty"`
	Text           string    `json:"text"`
	SenderID       int64     `json:"sender_id"`
	SenderUsername string    `json:"sender_username,omitempty"`
	SenderName     string    `json:"sender_name,omitempty"`
	Date           time.Time `json:"date"`
}

// --- Telegram API types (minimal subset) ---

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
}

type telegramUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *telegramMessage `json:"message,omitempty"`
	EditedMessage *telegramMessage `json:"edited_message,omitempty"`
}

type telegramMessage struct {
	MessageID int64        `json:"message_id"`
	From      *telegramUser `json:"from,omitempty"`
	Chat      telegramChat `json:"chat"`
	Date      int64        `json:"date"`
	Text      string       `json:"text"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

type telegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private, group, supergroup, channel
	Title string `json:"title,omitempty"`
}

// SendMessage sends a text message via the Telegram Bot API.
// Used by the agent service to deliver outbound messages.
func SendMessage(botToken string, chatID int64, text string, parseMode string) (int64, error) {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal sendMessage payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase, botToken)
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("failed to call sendMessage: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("failed to decode sendMessage response: %w", err)
	}

	if !result.OK {
		return 0, fmt.Errorf("sendMessage failed: %s", result.Description)
	}

	return result.Result.MessageID, nil
}
