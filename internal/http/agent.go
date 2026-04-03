package http

import (
	"net/http"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/agent"
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
