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
	defer resp.Body.Close()

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

// SendMessage sends a text message to a Slack channel via the Bot API.
func SendMessage(botToken string, channelID string, text string, threadTS string) (string, error) {
	payload := map[string]interface{}{
		"channel": channelID,
		"text":    text,
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
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
	defer resp.Body.Close()

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
