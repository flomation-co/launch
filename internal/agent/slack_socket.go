package agent

import (
	"sync"
	"time"

	appmetrics "flomation.app/automate/launch/internal/metrics"
	slackPkg "flomation.app/automate/launch/internal/slack"
	log "github.com/sirupsen/logrus"
)

// slackEventDedup prevents duplicate processing when Slack delivers both
// a "message" and "app_mention" event for the same user message. Keyed
// by agent_id + channel_id + timestamp (unique per Slack message).
var slackEventDedup = struct {
	sync.Mutex
	seen map[string]time.Time
}{seen: make(map[string]time.Time)}

const dedupWindow = 5 * time.Second

func slackEventKey(agentID, channelID, ts string) string {
	return agentID + ":" + channelID + ":" + ts
}

func isDuplicateSlackEvent(agentID, channelID, ts string) bool {
	key := slackEventKey(agentID, channelID, ts)
	now := time.Now()

	slackEventDedup.Lock()
	defer slackEventDedup.Unlock()

	// Prune stale entries periodically
	if len(slackEventDedup.seen) > 1000 {
		for k, t := range slackEventDedup.seen {
			if now.Sub(t) > dedupWindow {
				delete(slackEventDedup.seen, k)
			}
		}
	}

	if _, exists := slackEventDedup.seen[key]; exists {
		return true
	}
	slackEventDedup.seen[key] = now
	return false
}

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

	// Deduplicate: Slack sends both "message" and "app_mention" events for
	// the same @mention message. Only process the first one we see.
	if isDuplicateSlackEvent(agentID, msg.ChannelID, msg.Timestamp) {
		l.Debug("skipping duplicate slack event")
		return
	}

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

	appmetrics.InboundMessagesTotal.WithLabelValues("slack").Inc()
	l.Info("dispatching socket mode message")
	go func() { _ = s.HandleInboundMessage(agentID, inbound) }()
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
	go func() { _ = s.HandleInboundMessage(agentID, inbound) }()
}
