// Package slack handles Slack Events API webhook verification and message parsing.
// Slack uses a signing secret to verify requests rather than registering webhooks
// via API — the webhook URL is configured in the Slack App settings.
package slack

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	slackAPIBase    = "https://slack.com/api"
	httpTimeout     = 15 * time.Second
	timestampMaxAge = 5 * time.Minute
)

// Service manages Slack integrations for agents.
type Service struct{}

// NewService creates a Slack service.
func NewService() *Service {
	return &Service{}
}

// VerifyRequest validates the Slack signing secret on incoming requests.
// Returns the raw body if valid, error if not.
func VerifyRequest(signingSecret string, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	if timestamp == "" {
		return body, nil // no signing — skip verification (dev mode)
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}

	// Reject requests older than 5 minutes (replay protection)
	if time.Since(time.Unix(ts, 0)).Abs() > timestampMaxAge {
		return nil, fmt.Errorf("request timestamp too old")
	}

	sigBaseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(sigBaseString))
	expectedSig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	actualSig := r.Header.Get("X-Slack-Signature")
	if !hmac.Equal([]byte(expectedSig), []byte(actualSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	return body, nil
}

// ParseEvent extracts a message event from a Slack Events API payload.
// Returns nil if the payload is not a message event.
// Returns a URLVerification if Slack is verifying the endpoint.
func ParseEvent(body []byte) (*ParsedMessage, *URLVerification) {
	// First check if this is a url_verification challenge
	var envelope struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil
	}

	if envelope.Type == "url_verification" {
		return nil, &URLVerification{Challenge: envelope.Challenge}
	}

	if envelope.Type != "event_callback" {
		return nil, nil
	}

	var event struct {
		Event struct {
			Type     string `json:"type"`
			SubType  string `json:"subtype"`
			Text     string `json:"text"`
			User     string `json:"user"`
			Channel  string `json:"channel"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts,omitempty"`
			BotID    string `json:"bot_id,omitempty"`
		} `json:"event"`
		TeamID    string `json:"team_id"`
		EventID   string `json:"event_id"`
		EventTime int64  `json:"event_time"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, nil
	}

	// Only handle user messages (not bot messages, not subtypes like message_changed)
	if event.Event.Type != "message" && event.Event.Type != "app_mention" {
		return nil, nil
	}
	if event.Event.SubType != "" || event.Event.BotID != "" {
		return nil, nil // Skip bot messages and subtypes
	}

	return &ParsedMessage{
		Text:      event.Event.Text,
		UserID:    event.Event.User,
		ChannelID: event.Event.Channel,
		Timestamp: event.Event.TS,
		ThreadTS:  event.Event.ThreadTS,
		TeamID:    event.TeamID,
		EventID:   event.EventID,
		EventType: event.Event.Type,
	}, nil
}

// ParsedMessage contains extracted fields from a Slack event.
type ParsedMessage struct {
	Text      string `json:"text"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Timestamp string `json:"timestamp"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	TeamID    string `json:"team_id"`
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"` // message or app_mention
}

// URLVerification is returned when Slack sends a challenge for endpoint verification.
type URLVerification struct {
	Challenge string `json:"challenge"`
}

// UserInfo contains resolved user identity from Slack.
type UserInfo struct {
	DisplayName string // Short name (e.g. "Andy")
	RealName    string // Full name (e.g. "Andy Esser")
}

// LookupUser resolves a Slack user ID to their display and real names.
func LookupUser(botToken string, userID string) (*UserInfo, error) {
	req, err := http.NewRequest(http.MethodGet, slackAPIBase+"/users.info?user="+userID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		OK   bool `json:"ok"`
		User struct {
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("users.info failed: %s", result.Error)
	}

	return &UserInfo{
		DisplayName: result.User.Profile.DisplayName,
		RealName:    result.User.RealName,
	}, nil
}

// InteractionPayload represents a Slack interaction (button click, select
// menu, etc.) from Block Kit interactive components.
type InteractionPayload struct {
	Type string `json:"type"` // block_actions, view_submission, etc.
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	Channel struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	Message struct {
		TS   string `json:"ts"`
		Text string `json:"text"`
	} `json:"message"`
	Actions []InteractionAction `json:"actions"`
	// ResponseURL allows posting a follow-up message to the same channel.
	ResponseURL string `json:"response_url"`
	TriggerID   string `json:"trigger_id"`
	ThreadTS    string `json:"-"` // derived from message context
}

// InteractionAction is a single action the user took (button press, menu select).
type InteractionAction struct {
	Type     string `json:"type"`      // button, static_select, overflow, etc.
	ActionID string `json:"action_id"` // developer-defined ID
	BlockID  string `json:"block_id"`
	Text     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"text"`
	Value          string `json:"value,omitempty"` // button value
	SelectedOption *struct {
		Text  struct{ Text string } `json:"text"`
		Value string                `json:"value"`
	} `json:"selected_option,omitempty"` // select menu choice
}

// ParseInteraction parses a Slack interaction payload from a form-encoded request.
// Slack sends interactions as application/x-www-form-urlencoded with a "payload" field.
func ParseInteraction(body []byte) (*InteractionPayload, error) {
	var payload InteractionPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse interaction payload: %w", err)
	}
	return &payload, nil
}

// DescribeInteraction returns a human-readable summary of the interaction
// suitable for passing to the agent as an inbound message.
func DescribeInteraction(p *InteractionPayload) string {
	if len(p.Actions) == 0 {
		return "[User interacted with a message but no specific action was captured]"
	}

	action := p.Actions[0]
	switch action.Type {
	case "button":
		label := action.Text.Text
		if label == "" {
			label = action.Value
		}
		return fmt.Sprintf("[User clicked button: \"%s\" (value: %s)]", label, action.Value)
	case "static_select", "external_select":
		if action.SelectedOption != nil {
			return fmt.Sprintf("[User selected: \"%s\" (value: %s)]", action.SelectedOption.Text.Text, action.SelectedOption.Value)
		}
		return "[User made a selection]"
	case "overflow":
		if action.SelectedOption != nil {
			return fmt.Sprintf("[User chose overflow option: \"%s\"]", action.SelectedOption.Text.Text)
		}
		return "[User used overflow menu]"
	case "datepicker":
		return fmt.Sprintf("[User selected date: %s]", action.Value)
	default:
		return fmt.Sprintf("[User performed action: %s (value: %s)]", action.Type, action.Value)
	}
}

// RespondToInteraction sends a response via the response_url provided
// in the interaction payload. This updates or replaces the original message.
func RespondToInteraction(responseURL, text string, replaceOriginal bool) error {
	payload := map[string]interface{}{
		"text":             text,
		"replace_original": replaceOriginal,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Post(responseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to respond to interaction: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("interaction response returned %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

// SendMessage sends a message to a Slack channel via the Bot API.
// Text is sent with mrkdwn enabled by default. Optional blocks and
// attachments can be provided as pre-marshalled JSON arrays.
func SendMessage(botToken string, channelID string, text string, threadTS string, opts ...MessageOption) (string, error) {
	cfg := &messageConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	payload := map[string]interface{}{
		"channel": channelID,
		"text":    text,
		"mrkdwn":  true,
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	if cfg.blocks != nil {
		payload["blocks"] = cfg.blocks
	}
	if cfg.attachments != nil {
		payload["attachments"] = cfg.attachments
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, slackAPIBase+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var result struct {
		OK    bool   `json:"ok"`
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.OK {
		return "", fmt.Errorf("slack API error: %s", result.Error)
	}

	log.WithFields(log.Fields{
		"channel": channelID,
		"ts":      result.TS,
	}).Debug("slack message sent")

	return result.TS, nil
}

// messageConfig holds optional message parameters.
type messageConfig struct {
	blocks      interface{}
	attachments interface{}
}

// MessageOption configures optional fields on a Slack message.
type MessageOption func(*messageConfig)

// WithBlocks sets Block Kit blocks on the message.
func WithBlocks(blocks interface{}) MessageOption {
	return func(c *messageConfig) { c.blocks = blocks }
}

// WithAttachments sets attachments on the message.
func WithAttachments(attachments interface{}) MessageOption {
	return func(c *messageConfig) { c.attachments = attachments }
}
