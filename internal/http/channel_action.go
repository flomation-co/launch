package http

// Channel action handler — called by executor actions to perform
// channel-specific SDK operations like typing indicators.
//
// Route: POST /internal/agent/:agent_id/channel-action

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"flomation.app/automate/launch/internal/facebook"
	"flomation.app/automate/launch/internal/telegram"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type channelActionRequest struct {
	ChannelType string `json:"channel_type" binding:"required"`
	Action      string `json:"action" binding:"required"`
	ChatID      string `json:"chat_id"`
	ChannelID   string `json:"channel_id"`
	BotToken    string `json:"bot_token"`
}

func (s *Service) handleChannelAction(c *gin.Context) {
	agentID := c.Param("agent_id")

	var body channelActionRequest
	if err := c.BindJSON(&body); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	l := log.WithFields(log.Fields{
		"agent_id":     agentID,
		"channel_type": body.ChannelType,
		"action":       body.Action,
	})

	switch body.ChannelType {
	case "telegram":
		if err := s.handleTelegramAction(agentID, body); err != nil {
			l.WithError(err).Warn("telegram channel action failed")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	case "slack":
		// Slack doesn't support bot typing indicators via the API.
		c.JSON(http.StatusOK, gin.H{"status": "unsupported", "reason": "slack does not support bot typing indicators"})
		return
	case "facebook_messenger":
		// Messenger supports typing indicators via sender actions
		if body.Action == "typing" && body.ChannelID != "" {
			pageToken := s.getFacebookPageTokenForAgent(agentID)
			if pageToken != "" {
				_ = facebook.SendAction(pageToken, "", body.ChannelID, "typing_on")
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	default:
		c.JSON(http.StatusOK, gin.H{"status": "unsupported", "reason": "channel type does not support this action"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Service) handleTelegramAction(agentID string, body channelActionRequest) error {
	// Resolve the bot token — either from the request or from the agent's channel config.
	botToken := body.BotToken
	if botToken == "" {
		// Look up from the agent registration's channel config.
		reg, err := s.db.GetAgentRegistration(agentID)
		if err != nil || reg == nil {
			return fmt.Errorf("agent not registered")
		}
		botToken = extractTelegramBotToken(reg.Channels)
		if botToken == "" {
			return fmt.Errorf("no telegram bot token configured for agent")
		}
	}

	chatID := body.ChatID
	if chatID == "" {
		chatID = body.ChannelID
	}
	if chatID == "" {
		return fmt.Errorf("chat_id or channel_id is required")
	}

	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat_id: %w", err)
	}

	return telegram.SendChatAction(botToken, chatIDInt, body.Action)
}

// getFacebookPageTokenForAgent looks up the page access token from
// the agent's Facebook Messenger channel config.
func (s *Service) getFacebookPageTokenForAgent(agentID string) string {
	reg, err := s.db.GetAgentRegistration(agentID)
	if err != nil || reg == nil {
		return ""
	}
	var channelList []struct {
		Type   string `json:"type"`
		Config struct {
			PageAccessToken string `json:"page_access_token"`
		} `json:"config"`
	}
	if err := json.Unmarshal(reg.Channels, &channelList); err != nil {
		return ""
	}
	for _, ch := range channelList {
		if ch.Type == "facebook_messenger" && ch.Config.PageAccessToken != "" {
			return ch.Config.PageAccessToken
		}
	}
	return ""
}

// extractTelegramBotToken finds the Telegram bot token from an agent's
// channel configuration JSON.
func extractTelegramBotToken(channels json.RawMessage) string {
	if len(channels) == 0 {
		return ""
	}
	var channelList []struct {
		Type   string `json:"type"`
		Config struct {
			BotToken string `json:"bot_token"`
		} `json:"config"`
	}
	if err := json.Unmarshal(channels, &channelList); err != nil {
		return ""
	}
	for _, ch := range channelList {
		if ch.Type == "telegram" && ch.Config.BotToken != "" {
			return ch.Config.BotToken
		}
	}
	return ""
}
