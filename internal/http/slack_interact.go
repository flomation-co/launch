package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"flomation.app/automate/launch/internal/agent"
	slackpkg "flomation.app/automate/launch/internal/slack"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// handleSlackInteraction handles POST /webhook/slack/:agent_id/interact.
//
// Receives Slack interaction payloads from Block Kit interactive components
// (buttons, select menus, date pickers, overflow menus, etc.).
//
// Slack sends these as application/x-www-form-urlencoded with a "payload"
// JSON field. We must respond within 3 seconds.
//
// The interaction is forwarded to the agent as an inbound message describing
// what the user clicked, so the AI can respond naturally.
func (s *Service) handleSlackInteraction(c *gin.Context) {
	agentID := c.Param("agent_id")
	if err := uuid.Validate(agentID); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Slack sends interactions as form-encoded with a "payload" field.
	payloadStr := c.PostForm("payload")
	if payloadStr == "" {
		// Fallback: try reading raw body.
		body, _ := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		payloadStr = string(body)
	}

	if payloadStr == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	interaction, err := slackpkg.ParseInteraction([]byte(payloadStr))
	if err != nil {
		log.WithError(err).Warn("failed to parse slack interaction")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	l := log.WithFields(log.Fields{
		"agent_id": agentID,
		"type":     interaction.Type,
		"user_id":  interaction.User.ID,
		"channel":  interaction.Channel.ID,
	})

	// Acknowledge immediately — Slack requires a 200 within 3 seconds.
	c.Status(http.StatusOK)

	l.Info("slack interaction received")

	// Describe the interaction in natural language for the agent.
	description := slackpkg.DescribeInteraction(interaction)

	// Build structured action details for downstream processing.
	var actionDetails []string
	for _, a := range interaction.Actions {
		detail := map[string]string{
			"type":      a.Type,
			"action_id": a.ActionID,
			"block_id":  a.BlockID,
		}
		if a.Value != "" {
			detail["value"] = a.Value
		}
		if a.SelectedOption != nil {
			detail["selected_value"] = a.SelectedOption.Value
			detail["selected_text"] = a.SelectedOption.Text.Text
		}
		b, _ := json.Marshal(detail)
		actionDetails = append(actionDetails, string(b))
	}

	// Resolve user display name if possible.
	senderName := interaction.User.Name
	if senderName == "" {
		senderName = interaction.User.Username
	}
	if senderName == "" {
		senderName = interaction.User.ID
	}

	msg := agent.InboundMessage{
		ChannelType: "slack",
		Sender:      senderName,
		Content:     description,
		Metadata: map[string]interface{}{
			"channel_id":     interaction.Channel.ID,
			"user_id":        interaction.User.ID,
			"user_name":      senderName,
			"display_name":   senderName,
			"chat_id":        interaction.Channel.ID,
			"thread_ts":      interaction.Message.TS,
			"thread_id":      interaction.Message.TS,
			"interaction":    true,
			"response_url":   interaction.ResponseURL,
			"trigger_id":     interaction.TriggerID,
			"action_details": strings.Join(actionDetails, ","),
		},
	}

	// Dispatch to the agent asynchronously.
	go func() {
		if err := s.agent.HandleInboundMessage(agentID, msg); err != nil {
			l.WithError(err).Error("failed to handle slack interaction")
		}
	}()
}
