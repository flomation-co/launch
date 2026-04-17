package http

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/agent"
	slackpkg "flomation.app/automate/launch/internal/slack"
	"flomation.app/automate/launch/internal/telegram"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// registerAgent handles POST /agent/:id — called by the API when an agent is started.
func (s *Service) registerAgent(c *gin.Context) {
	id := c.Param("id")
	if err := uuid.Validate(id); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var reg launch.AgentRegistration
	if err := c.ShouldBindJSON(&reg); err != nil {
		log.WithFields(log.Fields{"error": err}).Error("unable to bind agent registration")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	reg.AgentID = id

	if err := s.agent.RegisterAgent(reg); err != nil {
		log.WithFields(log.Fields{"error": err, "agent_id": id}).Error("unable to register agent")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "registered"})
}

// deregisterAgent handles DELETE /agent/:id — called by the API when an agent is stopped.
func (s *Service) deregisterAgent(c *gin.Context) {
	id := c.Param("id")
	if err := uuid.Validate(id); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if err := s.agent.DeregisterAgent(id); err != nil {
		log.WithFields(log.Fields{"error": err, "agent_id": id}).Error("unable to deregister agent")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deregistered"})
}

// handleAgentWebhook handles POST /webhook/agent/:agent_id — receives messages from external sources.
func (s *Service) handleAgentWebhook(c *gin.Context) {
	agentID := c.Param("agent_id")
	if err := uuid.Validate(agentID); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var body map[string]interface{}
	_ = c.ShouldBindJSON(&body)

	// Build inbound message from webhook payload
	msg := agent.InboundMessage{
		ChannelType: "webhook",
		Sender:      c.ClientIP(),
		Metadata:    body,
	}

	// Use "content" field if present, otherwise serialise entire body
	if content, ok := body["content"].(string); ok {
		msg.Content = content
	} else if body != nil {
		// Use the whole payload as content for generic webhooks
		msg.Content = "Webhook received"
		msg.Metadata = body
	}

	// Dispatch asynchronously so webhook responds quickly
	go func() {
		if err := s.agent.HandleInboundMessage(agentID, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Error("failed to handle agent webhook")
		}
	}()

	c.JSON(http.StatusOK, gin.H{"status": "received"})
}

// handleTelegramWebhook handles POST /webhook/telegram/:agent_id — receives Telegram Bot API updates.
func (s *Service) handleTelegramWebhook(c *gin.Context) {
	agentID := c.Param("agent_id")
	if err := uuid.Validate(agentID); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20)) // 1 MB limit
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	parsed := telegram.ParseUpdate(body)
	if parsed == nil {
		// Not a message update (could be callback_query, etc.) — acknowledge silently
		c.Status(http.StatusOK)
		return
	}

	// Build sender string for display
	sender := parsed.SenderName
	if sender == "" && parsed.SenderUsername != "" {
		sender = "@" + parsed.SenderUsername
	}
	if sender == "" {
		sender = fmt.Sprintf("user:%d", parsed.SenderID)
	}

	chatID := strconv.FormatInt(parsed.ChatID, 10)
	metadata := map[string]interface{}{
		// Canonical keys (consistent across all providers)
		"channel_id": chatID,
		"user_id":    fmt.Sprintf("%d", parsed.SenderID),
		"user_name":  parsed.SenderName,
		// Provider-specific keys (kept for backwards compatibility)
		"message_id":      fmt.Sprintf("%d", parsed.MessageID),
		"chat_id":         chatID,
		"chat_type":       parsed.ChatType,
		"chat_title":      parsed.ChatTitle,
		"sender_id":       fmt.Sprintf("%d", parsed.SenderID),
		"sender_username": parsed.SenderUsername,
		"sender_name":     parsed.SenderName,
		"date":            parsed.Date.Format("2006-01-02T15:04:05Z"),
	}

	// Voice message handling: download the audio file and include as base64
	if parsed.IsVoice && parsed.VoiceFileID != "" {
		metadata["is_voice"] = true
		metadata["voice_duration"] = parsed.VoiceDuration
		metadata["voice_mime_type"] = parsed.VoiceMimeType

		reg, regErr := s.agent.GetRegistration(agentID)
		botToken := ""
		if regErr == nil && reg != nil && reg.Channels != nil {
			botToken = extractTelegramBotToken(reg.Channels)
		}
		if botToken != "" {
			audioData, err := telegram.DownloadFile(botToken, parsed.VoiceFileID)
			if err != nil {
				log.WithFields(log.Fields{
					"agent_id": agentID,
					"error":    err,
					"file_id":  parsed.VoiceFileID,
				}).Warn("failed to download telegram voice file")
			} else {
				metadata["voice_audio_base64"] = base64.StdEncoding.EncodeToString(audioData)
				metadata["voice_audio_size"] = len(audioData)
				log.WithFields(log.Fields{
					"agent_id":   agentID,
					"duration_s": parsed.VoiceDuration,
					"size_bytes": len(audioData),
				}).Info("downloaded telegram voice message")
			}
		}
	}

	channelType := "telegram"
	if parsed.IsVoice {
		channelType = "telegram_voice"
	}

	msg := agent.InboundMessage{
		ChannelType: channelType,
		Sender:      sender,
		Content:     parsed.Text,
		Metadata:    metadata,
	}

	// Dispatch asynchronously — Telegram expects a quick 200 response
	go func() {
		if err := s.agent.HandleInboundMessage(agentID, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
				"chat_id":  parsed.ChatID,
			}).Error("failed to handle telegram message")
		}
	}()

	c.Status(http.StatusOK)
}

// handleSlackWebhook handles POST /webhook/slack/:agent_id — receives Slack Events API payloads.
func (s *Service) handleSlackWebhook(c *gin.Context) {
	agentID := c.Param("agent_id")
	if err := uuid.Validate(agentID); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Parse the event — handle url_verification challenge first
	parsed, verification := slackpkg.ParseEvent(body)

	if verification != nil {
		// Slack is verifying the endpoint — respond with the challenge
		c.JSON(http.StatusOK, gin.H{"challenge": verification.Challenge})
		return
	}

	if parsed == nil {
		// Not a message event — acknowledge silently
		c.Status(http.StatusOK)
		return
	}

	// Resolve user names from Slack API if possible
	senderDisplay := parsed.UserID
	senderReal := parsed.UserID
	reg, _ := s.agent.GetRegistration(agentID)
	if reg != nil {
		if botToken := extractSlackBotToken(reg.Channels); botToken != "" {
			if info, err := slackpkg.LookupUser(botToken, parsed.UserID); err == nil && info != nil {
				if info.DisplayName != "" {
					senderDisplay = info.DisplayName
				}
				if info.RealName != "" {
					senderReal = info.RealName
				}
			}
		}
	}

	msg := agent.InboundMessage{
		ChannelType: "slack",
		Sender:      senderDisplay,
		Content:     parsed.Text,
		Metadata: map[string]interface{}{
			// Canonical keys (consistent across all providers)
			"channel_id": parsed.ChannelID,
			"thread_id":  parsed.ThreadTS,
			"user_id":    parsed.UserID,
			"user_name":  senderDisplay,
			// Provider-specific keys (kept for backwards compatibility)
			"display_name": senderDisplay,
			"real_name":    senderReal,
			"timestamp":    parsed.Timestamp,
			"thread_ts":    parsed.ThreadTS,
			"team_id":      parsed.TeamID,
			"event_id":     parsed.EventID,
			"event_type":   parsed.EventType,
			// Alias: Telegram flows that use chat_id will also work
			"chat_id": parsed.ChannelID,
		},
	}

	// Dispatch asynchronously — Slack expects a quick 200
	go func() {
		if err := s.agent.HandleInboundMessage(agentID, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
				"channel":  parsed.ChannelID,
			}).Error("failed to handle slack message")
		}
	}()

	c.Status(http.StatusOK)
}

// extractSlackBotToken finds the bot_token from a Slack channel in the agent's channel config.
func extractSlackBotToken(channelsRaw json.RawMessage) string {
	var channels []struct {
		Type   string `json:"type"`
		Config struct {
			BotToken string `json:"bot_token"`
		} `json:"config"`
	}
	if err := json.Unmarshal(channelsRaw, &channels); err != nil {
		return ""
	}
	for _, ch := range channels {
		if ch.Type == "slack" && ch.Config.BotToken != "" {
			return ch.Config.BotToken
		}
	}
	return ""
}
