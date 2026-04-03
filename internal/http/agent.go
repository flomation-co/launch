package http

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/agent"
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

	msg := agent.InboundMessage{
		ChannelType: "telegram",
		Sender:      sender,
		Content:     parsed.Text,
		Metadata: map[string]interface{}{
			"message_id":      fmt.Sprintf("%d", parsed.MessageID),
			"chat_id":         strconv.FormatInt(parsed.ChatID, 10),
			"chat_type":       parsed.ChatType,
			"chat_title":      parsed.ChatTitle,
			"sender_id":       fmt.Sprintf("%d", parsed.SenderID),
			"sender_username": parsed.SenderUsername,
			"sender_name":     parsed.SenderName,
			"date":            parsed.Date.Format("2006-01-02T15:04:05Z"),
		},
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
