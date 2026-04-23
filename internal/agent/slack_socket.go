package agent

import (
	slackPkg "flomation.app/automate/launch/internal/slack"
	log "github.com/sirupsen/logrus"
)

// handleSlackSocketMessage converts a Socket Mode message event into an
// InboundMessage and dispatches it through the standard agent pipeline.
func (s *Service) handleSlackSocketMessage(agentID, botToken string, msg *slackPkg.ParsedMessage) {
	l := log.WithFields(log.Fields{
		"agent_id":   agentID,
		"channel_id": msg.ChannelID,
		"user_id":    msg.UserID,
		"event_type": msg.EventType,
		"source":     "socket_mode",
	})

	// Resolve user display name
	senderName := msg.UserID
	if info, err := slackPkg.LookupUser(botToken, msg.UserID); err == nil && info != nil {
		if info.DisplayName != "" {
			senderName = info.DisplayName
		} else if info.RealName != "" {
			senderName = info.RealName
		}
	}

	metadata := map[string]interface{}{
		"channel_id": msg.ChannelID,
		"user_id":    msg.UserID,
		"user_name":  senderName,
		"timestamp":  msg.Timestamp,
		"team_id":    msg.TeamID,
		"event_id":   msg.EventID,
		"event_type": msg.EventType,
	}
	if msg.ThreadTS != "" {
		metadata["thread_id"] = msg.ThreadTS
		metadata["thread_ts"] = msg.ThreadTS
	}

	inbound := InboundMessage{
		ChannelType: "slack",
		Sender:      senderName,
		Content:     msg.Text,
		Metadata:    metadata,
	}

	l.Info("dispatching socket mode message")
	go s.HandleInboundMessage(agentID, inbound)
}

// handleSlackSocketInteraction converts a Socket Mode interaction event
// into an InboundMessage describing the user's action.
func (s *Service) handleSlackSocketInteraction(agentID, botToken string, payload *slackPkg.InteractionPayload) {
	l := log.WithFields(log.Fields{
		"agent_id":   agentID,
		"channel_id": payload.Channel.ID,
		"user_id":    payload.User.ID,
		"source":     "socket_mode",
	})

	description := slackPkg.DescribeInteraction(payload)

	threadTS := payload.Message.TS
	metadata := map[string]interface{}{
		"channel_id":   payload.Channel.ID,
		"user_id":      payload.User.ID,
		"user_name":    payload.User.Name,
		"timestamp":    payload.Message.TS,
		"interaction":  true,
		"response_url": payload.ResponseURL,
		"trigger_id":   payload.TriggerID,
	}
	if threadTS != "" {
		metadata["thread_id"] = threadTS
		metadata["thread_ts"] = threadTS
	}

	inbound := InboundMessage{
		ChannelType: "slack",
		Sender:      payload.User.Name,
		Content:     description,
		Metadata:    metadata,
	}

	l.Info("dispatching socket mode interaction")
	go s.HandleInboundMessage(agentID, inbound)
}
