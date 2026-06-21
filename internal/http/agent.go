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
	appmetrics "flomation.app/automate/launch/internal/metrics"
	slackpkg "flomation.app/automate/launch/internal/slack"
	teamspkg "flomation.app/automate/launch/internal/teams"
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

// handleTelegramWebhook handles POST /webhook/telegram/:id — receives
// Telegram Bot API updates. The :id is a trigger_id for new
// trigger-keyed registrations or an agent_id for legacy agent-scoped
// registrations; the handler tries the trigger lookup first, falls back
// to the agent dispatch path if not found.
func (s *Service) handleTelegramWebhook(c *gin.Context) {
	id := c.Param("id")
	if err := uuid.Validate(id); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Routing decision: is :id a known trigger?
	tr, _ := s.trigger.GetTriggerByID(id)
	isTrigger := tr != nil && tr.Type == launch.TriggerTypeTelegram

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

		var botToken string
		if isTrigger {
			botToken = s.resolveTriggerCreds(id)["bot_token"]
		} else {
			botToken = s.resolveTelegramCreds(id)["bot_token"]
		}
		if botToken != "" {
			audioData, err := telegram.DownloadFile(botToken, parsed.VoiceFileID)
			if err != nil {
				log.WithFields(log.Fields{
					"id":      id,
					"error":   err,
					"file_id": parsed.VoiceFileID,
				}).Warn("failed to download telegram voice file")
			} else {
				metadata["voice_audio_base64"] = base64.StdEncoding.EncodeToString(audioData)
				metadata["voice_audio_size"] = len(audioData)
				log.WithFields(log.Fields{
					"id":         id,
					"duration_s": parsed.VoiceDuration,
					"size_bytes": len(audioData),
				}).Info("downloaded telegram voice message")
			}
		}
	}

	// Non-voice attachments (photos, documents, videos). Each is
	// downloaded inline and forwarded to the API as base64 bytes
	// alongside their metadata. The API uploads to the blob tier
	// server-side (where org_id is known) and replaces the base64
	// with a flo:blob:... token before the agent_message is written.
	// Launch never touches the blob store directly — keeps the auth
	// surface single-sourced.
	if len(parsed.Attachments) > 0 {
		var botToken string
		if isTrigger {
			botToken = s.resolveTriggerCreds(id)["bot_token"]
		} else {
			botToken = s.resolveTelegramCreds(id)["bot_token"]
		}
		if botToken != "" {
			inbound := make([]map[string]interface{}, 0, len(parsed.Attachments))
			for _, att := range parsed.Attachments {
				bytes, err := telegram.DownloadFile(botToken, att.FileID)
				if err != nil {
					log.WithFields(log.Fields{
						"id":      id,
						"file_id": att.FileID,
						"kind":    att.Kind,
						"error":   err,
					}).Warn("failed to download telegram attachment")
					continue
				}
				entry := map[string]interface{}{
					"name":           att.Name,
					"mime":           att.Mime,
					"size":           len(bytes),
					"kind":           att.Kind,
					"source_id":      att.FileID,
					"source_kind":    "telegram",
					"content_base64": base64.StdEncoding.EncodeToString(bytes),
				}
				if att.Width > 0 {
					entry["width"] = att.Width
				}
				if att.Height > 0 {
					entry["height"] = att.Height
				}
				if att.Duration > 0 {
					entry["duration"] = att.Duration
				}
				inbound = append(inbound, entry)
				log.WithFields(log.Fields{
					"id":         id,
					"kind":       att.Kind,
					"name":       att.Name,
					"size_bytes": len(bytes),
				}).Info("downloaded telegram attachment")
			}
			if len(inbound) > 0 {
				metadata["inbound_attachments"] = inbound
			}
		}
	}

	channelType := "telegram"
	if parsed.IsVoice {
		channelType = "telegram_voice"
	}

	// Dispatch asynchronously — Telegram expects a quick 200 response
	appmetrics.InboundMessagesTotal.WithLabelValues("telegram").Inc()
	go func() {
		if isTrigger {
			s.postTriggerDispatch(id, channelType, sender, parsed.Text, metadata)
			return
		}
		// Legacy agent-keyed fallback.
		msg := agent.InboundMessage{
			ChannelType: channelType,
			Sender:      sender,
			Content:     parsed.Text,
			Metadata:    metadata,
		}
		if err := s.agent.HandleInboundMessage(id, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": id,
				"error":    err,
				"chat_id":  parsed.ChatID,
			}).Error("failed to handle telegram message")
		}
	}()

	c.Status(http.StatusOK)
}

// handleSlackWebhook handles POST /webhook/slack/:id — receives Slack
// Events API payloads. :id is a trigger_id for new registrations or an
// agent_id for legacy.
func (s *Service) handleSlackWebhook(c *gin.Context) {
	id := c.Param("id")
	if err := uuid.Validate(id); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, _ := s.trigger.GetTriggerByID(id)
	isTrigger := tr != nil && tr.Type == "slack"

	var creds map[string]string
	if isTrigger {
		creds = s.resolveTriggerCreds(id)
	} else {
		creds = s.resolveSlackCreds(id)
	}
	botToken := creds["bot_token"]
	signingSecret := creds["signing_secret"]

	// Slack signing-secret request verification (replay-protected HMAC of body).
	// When the agent has a signing_secret configured we MUST verify before
	// trusting the payload. When unset (dev/local mode) we read the body raw.
	var body []byte
	if signingSecret != "" {
		verified, err := slackpkg.VerifyRequest(signingSecret, c.Request)
		if err != nil {
			log.WithFields(log.Fields{
				"id":    id,
				"error": err,
			}).Warn("slack signature verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		body = verified
	} else {
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
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
	if botToken != "" {
		if info, err := slackpkg.LookupUser(botToken, parsed.UserID); err == nil && info != nil {
			if info.DisplayName != "" {
				senderDisplay = info.DisplayName
			}
			if info.RealName != "" {
				senderReal = info.RealName
			}
		}
	}

	metadata := map[string]interface{}{
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
	}

	// File attachments (file_share events). Each url_private requires
	// a Bearer-auth GET to download — Slack's auth model is different
	// from Telegram's public file URLs, but the resulting shape
	// forwarded to the API is identical: the same inbound_attachments
	// array M2 introduced. A missing bot_token (rare in production but
	// possible in dev) skips the download with a logged warning rather
	// than dropping the whole message.
	if len(parsed.Attachments) > 0 && botToken != "" {
		inbound := make([]map[string]interface{}, 0, len(parsed.Attachments))
		for _, att := range parsed.Attachments {
			bytes, err := slackpkg.DownloadFile(botToken, att.URLPrivate)
			if err != nil {
				log.WithFields(log.Fields{
					"id":      id,
					"file_id": att.FileID,
					"kind":    att.Kind,
					"error":   err,
				}).Warn("failed to download slack attachment")
				continue
			}
			inbound = append(inbound, map[string]interface{}{
				"name":           att.Name,
				"mime":           att.Mime,
				"size":           len(bytes),
				"kind":           att.Kind,
				"source_id":      att.FileID,
				"source_kind":    "slack",
				"content_base64": base64.StdEncoding.EncodeToString(bytes),
			})
			log.WithFields(log.Fields{
				"id":         id,
				"kind":       att.Kind,
				"name":       att.Name,
				"size_bytes": len(bytes),
			}).Info("downloaded slack attachment")
		}
		if len(inbound) > 0 {
			metadata["inbound_attachments"] = inbound
		}
	}

	// Dispatch asynchronously — Slack expects a quick 200
	appmetrics.InboundMessagesTotal.WithLabelValues("slack").Inc()
	go func() {
		if isTrigger {
			s.postTriggerDispatch(id, "slack", senderDisplay, parsed.Text, metadata)
			return
		}
		msg := agent.InboundMessage{
			ChannelType: "slack",
			Sender:      senderDisplay,
			Content:     parsed.Text,
			Metadata:    metadata,
		}
		if err := s.agent.HandleInboundMessage(id, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": id,
				"error":    err,
				"channel":  parsed.ChannelID,
			}).Error("failed to handle slack message")
		}
	}()

	c.Status(http.StatusOK)
}

// resolveTelegramCreds returns the resolved Telegram credentials for an agent.
// Same dual-read pattern as resolveSlackCreds.
func (s *Service) resolveTelegramCreds(agentID string) map[string]string {
	if creds, ok := s.resolveChannelCreds(agentID, "telegram"); ok {
		return creds
	}
	reg, err := s.agent.GetRegistration(agentID)
	if err != nil || reg == nil || reg.Channels == nil {
		return map[string]string{}
	}
	return parseLegacyChannelConfig(reg.Channels, "telegram")
}

// resolveSlackCreds returns the resolved Slack credentials for an agent.
// Tries the new trigger-node config first, then falls back to the legacy
// agent.channels store. Always returns a non-nil map (empty if unconfigured).
func (s *Service) resolveSlackCreds(agentID string) map[string]string {
	if creds, ok := s.resolveChannelCreds(agentID, "slack"); ok {
		return creds
	}
	reg, err := s.agent.GetRegistration(agentID)
	if err != nil || reg == nil || reg.Channels == nil {
		return map[string]string{}
	}
	return parseLegacyChannelConfig(reg.Channels, "slack")
}

// parseLegacyChannelConfig walks the legacy agent.channels JSON array and
// returns the config map for the first entry whose type matches.
func parseLegacyChannelConfig(channelsRaw json.RawMessage, channelType string) map[string]string {
	out := map[string]string{}
	var channels []struct {
		Type   string                 `json:"type"`
		Config map[string]interface{} `json:"config"`
	}
	if err := json.Unmarshal(channelsRaw, &channels); err != nil {
		return out
	}
	for _, ch := range channels {
		if ch.Type != channelType {
			continue
		}
		for k, v := range ch.Config {
			if str, ok := v.(string); ok {
				out[k] = str
			}
		}
		return out
	}
	return out
}

// ── Teams Bot Framework webhook ─────────────────────────────────────

func (s *Service) handleTeamsWebhook(c *gin.Context) {
	id := c.Param("id")

	tr, _ := s.trigger.GetTriggerByID(id)
	isTrigger := tr != nil && tr.Type == launch.TriggerTypeTeams

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		log.WithError(err).Error("[teams] unable to read body")
		c.Status(http.StatusBadRequest)
		return
	}

	parsed, activity, err := teamspkg.ParseActivity(body)
	if err != nil {
		log.WithError(err).Error("[teams] unable to parse activity")
		c.Status(http.StatusBadRequest)
		return
	}

	// Non-message activities (e.g., conversationUpdate, typing) — acknowledge silently
	if parsed == nil {
		c.Status(http.StatusOK)
		return
	}

	log.WithFields(log.Fields{
		"id":           id,
		"user":         parsed.UserName,
		"conversation": parsed.ConversationType,
		"text_preview": truncateText(parsed.Text, 50),
	}).Info("[teams] received message")

	// Send typing indicator
	if s.config.Microsoft != nil {
		go func() {
			token, err := teamspkg.GetBotToken(s.config.Microsoft.ClientID, s.config.Microsoft.ClientSecret)
			if err != nil {
				log.WithError(err).Debug("[teams] unable to get bot token for typing indicator")
				return
			}
			teamspkg.SendTypingIndicator(parsed.ServiceURL, parsed.ConversationID, s.config.Microsoft.ClientID, token)
		}()
	}

	metadata := map[string]interface{}{
		// Canonical keys (consistent across all providers)
		"channel_id": parsed.ConversationID,
		"user_id":    parsed.UserID,
		"user_name":  parsed.UserName,
		// Provider-specific keys
		"conversation_type": parsed.ConversationType,
		"teams_channel_id":  parsed.ChannelID,
		"teams_team_id":     parsed.TeamID,
		"activity_id":       parsed.ActivityID,
		"service_url":       parsed.ServiceURL,
		"tenant_id":         parsed.TenantID,
		// Alias for cross-provider compatibility
		"chat_id": parsed.ConversationID,
	}

	_ = activity // reserved for future use (e.g., adaptive card handling)

	// Dispatch asynchronously — Bot Framework expects a quick 200/201
	appmetrics.InboundMessagesTotal.WithLabelValues("teams").Inc()
	go func() {
		if isTrigger {
			s.postTriggerDispatch(id, "teams", parsed.UserName, parsed.Text, metadata)
			return
		}
		msg := agent.InboundMessage{
			ChannelType: "teams",
			Sender:      parsed.UserName,
			Content:     parsed.Text,
			Metadata:    metadata,
		}
		if err := s.agent.HandleInboundMessage(id, msg); err != nil {
			log.WithFields(log.Fields{
				"agent_id": id,
				"error":    err,
				"user":     parsed.UserName,
			}).Error("[teams] failed to handle message")
		}
	}()

	c.Status(http.StatusCreated) // Bot Framework expects 201 for successful processing
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
