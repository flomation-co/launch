package http

// Identity verification dispatch — called by the API when the first side
// of an identity link is confirmed. Dispatches the orchestrator flow on
// the target channel to request second-side confirmation.
//
// Route: POST /internal/agent/:agent_id/verify-identity

import (
	"net/http"

	"flomation.app/automate/launch/internal/agent"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type verifyIdentityRequest struct {
	TargetUserID    string `json:"target_user_id" binding:"required"`
	TargetChannel   string `json:"target_channel_type" binding:"required"`
	TargetChannelID string `json:"target_channel_id" binding:"required"`
	TargetExternal  string `json:"target_external_id"`
	SourceChannel   string `json:"source_channel_type" binding:"required"`
	Content         string `json:"content"`
}

func (s *Service) handleVerifyIdentity(c *gin.Context) {
	agentID := c.Param("agent_id")

	var body verifyIdentityRequest
	if err := c.BindJSON(&body); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if body.Content == "" {
		body.Content = "[IDENTITY VERIFICATION] A user on " + body.SourceChannel +
			" claims to also be you. Please confirm or deny this link. " +
			"If this is you, reply 'yes' to connect your conversations across channels. " +
			"If not, reply 'no' and the link will be cancelled."
	}

	// Build metadata with channel-specific identifiers so that
	// deriveExternalID and deriveChannelScope in the agent service
	// correctly resolve identity and conversation for the target user.
	metadata := map[string]interface{}{
		"trigger_source": "identity_verification",
		"target_user_id": body.TargetUserID,
		"source_channel": body.SourceChannel,
	}

	switch body.TargetChannel {
	case "slack":
		metadata["user_id"] = body.TargetExternal
		metadata["channel_id"] = body.TargetChannelID
	case "telegram":
		metadata["sender_id"] = body.TargetExternal
		metadata["chat_id"] = body.TargetChannelID
		metadata["channel_id"] = body.TargetChannelID
	case "email":
		metadata["from"] = body.TargetExternal
	}

	msg := agent.InboundMessage{
		ChannelType: body.TargetChannel,
		Sender:      body.TargetExternal,
		Content:     body.Content,
		Metadata:    metadata,
	}

	go func() {
		if err := s.agent.HandleInboundMessage(agentID, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id":       agentID,
				"target_channel": body.TargetChannel,
				"error":          err,
			}).Error("failed to dispatch identity verification")
		}
	}()

	c.JSON(http.StatusOK, gin.H{"status": "dispatched"})
}
