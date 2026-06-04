// Package teams provides Microsoft Teams Bot Framework message parsing,
// verification, and response helpers. It handles incoming Activity objects
// from the Bot Framework and provides methods for sending replies.
package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Activity represents an incoming Bot Framework activity.
// See https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-activity
type Activity struct {
	Type         string       `json:"type"`
	ID           string       `json:"id"`
	Timestamp    string       `json:"timestamp"`
	ChannelID    string       `json:"channelId"`
	ServiceURL   string       `json:"serviceUrl"`
	From         ChannelAccount `json:"from"`
	Conversation ConversationAccount `json:"conversation"`
	Recipient    ChannelAccount `json:"recipient"`
	Text         string       `json:"text"`
	TextFormat   string       `json:"textFormat"`
	ChannelData  json.RawMessage `json:"channelData,omitempty"`

	// Teams-specific fields
	TeamsChannelID string `json:"-"` // Extracted from channelData
	TeamsTeamID    string `json:"-"` // Extracted from channelData
}

// ChannelAccount identifies a user or bot in a conversation.
type ChannelAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	AADObjectID string `json:"aadObjectId,omitempty"`
}

// ConversationAccount identifies a conversation.
type ConversationAccount struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	ConversationType string `json:"conversationType,omitempty"` // personal, groupChat, channel
	IsGroup          bool   `json:"isGroup,omitempty"`
	TenantID         string `json:"tenantId,omitempty"`
}

// ParsedMessage is the normalised output from parsing a Bot Framework activity.
type ParsedMessage struct {
	Text             string
	UserID           string // AAD object ID or from.id
	UserName         string // Display name
	ConversationID   string
	ConversationType string // personal, groupChat, channel
	ChannelID        string // Teams channel ID (for channel messages)
	TeamID           string // Teams team ID (for channel messages)
	ActivityID       string // For reply threading
	ServiceURL       string // Required for sending replies
	TenantID         string
}

// teamsChannelData holds Teams-specific channel data fields.
type teamsChannelData struct {
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	Tenant struct {
		ID string `json:"id"`
	} `json:"tenant"`
}

// ParseActivity parses an incoming Bot Framework Activity into a normalised message.
// Returns nil if the activity is not a message type or should be ignored.
func ParseActivity(body []byte) (*ParsedMessage, *Activity, error) {
	var activity Activity
	if err := json.Unmarshal(body, &activity); err != nil {
		return nil, nil, fmt.Errorf("parse activity: %w", err)
	}

	// Only process message activities
	if activity.Type != "message" {
		return nil, &activity, nil
	}

	// Skip empty messages (e.g., card actions without text)
	text := strings.TrimSpace(activity.Text)
	if text == "" {
		return nil, &activity, nil
	}

	// Strip bot mentions from text (Teams includes <at>BotName</at> in channel messages)
	text = stripBotMentions(text)
	if text == "" {
		return nil, &activity, nil
	}

	// Extract Teams-specific channel data
	var channelData teamsChannelData
	if activity.ChannelData != nil {
		_ = json.Unmarshal(activity.ChannelData, &channelData)
	}

	userID := activity.From.AADObjectID
	if userID == "" {
		userID = activity.From.ID
	}

	tenantID := activity.Conversation.TenantID
	if tenantID == "" {
		tenantID = channelData.Tenant.ID
	}

	msg := &ParsedMessage{
		Text:             text,
		UserID:           userID,
		UserName:         activity.From.Name,
		ConversationID:   activity.Conversation.ID,
		ConversationType: activity.Conversation.ConversationType,
		ChannelID:        channelData.Channel.ID,
		TeamID:           channelData.Team.ID,
		ActivityID:       activity.ID,
		ServiceURL:       activity.ServiceURL,
		TenantID:         tenantID,
	}

	return msg, &activity, nil
}

// stripBotMentions removes <at>BotName</at> tags from Teams messages.
func stripBotMentions(text string) string {
	for {
		start := strings.Index(text, "<at>")
		if start == -1 {
			break
		}
		end := strings.Index(text, "</at>")
		if end == -1 {
			break
		}
		text = text[:start] + text[end+5:]
	}
	return strings.TrimSpace(text)
}

// SendReply sends a reply to an incoming activity using the Bot Framework REST API.
// The serviceURL from the original activity is used as the base URL.
func SendReply(serviceURL, conversationID, activityID, appID, token, text string) error {
	reply := map[string]interface{}{
		"type": "message",
		"from": map[string]string{
			"id": appID,
		},
		"conversation": map[string]string{
			"id": conversationID,
		},
		"text":       text,
		"textFormat": "markdown",
	}

	body, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("marshal reply: %w", err)
	}

	// Reply to the specific activity (threaded)
	endpoint := fmt.Sprintf("%sv3/conversations/%s/activities/%s",
		ensureTrailingSlash(serviceURL), conversationID, activityID)

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("reply returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// SendTypingIndicator sends a typing activity to show the bot is processing.
func SendTypingIndicator(serviceURL, conversationID, appID, token string) {
	typing := map[string]interface{}{
		"type": "typing",
		"from": map[string]string{
			"id": appID,
		},
		"conversation": map[string]string{
			"id": conversationID,
		},
	}

	body, err := json.Marshal(typing)
	if err != nil {
		return
	}

	endpoint := fmt.Sprintf("%sv3/conversations/%s/activities",
		ensureTrailingSlash(serviceURL), conversationID)

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.WithError(err).Debug("[teams] failed to send typing indicator")
		return
	}
	_ = resp.Body.Close()
}

// GetBotToken retrieves an access token for the Bot Framework using client credentials.
func GetBotToken(appID, appPassword string) (string, error) {
	data := fmt.Sprintf("grant_type=client_credentials&client_id=%s&client_secret=%s&scope=https%%3A%%2F%%2Fapi.botframework.com%%2F.default",
		appID, appPassword)

	resp, err := http.Post(
		"https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(data),
	)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}

	return result.AccessToken, nil
}

func ensureTrailingSlash(url string) string {
	if strings.HasSuffix(url, "/") {
		return url
	}
	return url + "/"
}
