package http

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/agent"
	"flomation.app/automate/launch/internal/facebook"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleFacebookVerification handles GET /webhook/facebook.
// Facebook sends a hub.challenge verification request when configuring
// the webhook URL in the App Dashboard.
func (s *Service) handleFacebookVerification(c *gin.Context) {
	mode := c.Query("hub.mode")
	token := c.Query("hub.verify_token")
	challenge := c.Query("hub.challenge")

	if s.config.Facebook == nil || s.config.Facebook.VerifyToken == "" {
		log.Warn("Facebook webhook verification attempted but no verify_token configured")
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	if mode == "subscribe" && token == s.config.Facebook.VerifyToken {
		log.Info("Facebook webhook verification successful")
		c.String(http.StatusOK, challenge)
		return
	}

	log.WithFields(log.Fields{
		"mode":  mode,
		"token": token,
	}).Warn("Facebook webhook verification failed")
	c.AbortWithStatus(http.StatusForbidden)
}

// handleFacebookWebhook handles POST /webhook/facebook.
// All Facebook Page events (Messenger messages, feed changes) arrive here.
// Events are demultiplexed by page ID using the PageIndex.
func (s *Service) handleFacebookWebhook(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Verify signature
	if s.config.Facebook != nil && s.config.Facebook.AppSecret != "" {
		if err := facebook.VerifySignature(s.config.Facebook.AppSecret, body, c.Request); err != nil {
			log.WithError(err).Warn("Facebook webhook signature verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	// Parse envelope
	env, err := facebook.ParseEnvelope(body)
	if err != nil {
		log.WithError(err).Warn("Facebook webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Process each entry asynchronously
	for _, entry := range env.Entry {
		pageID := entry.ID

		messages := facebook.ParseMessengerEvents(entry)
		feedEvents := facebook.ParseFeedEvents(entry)

		log.WithFields(log.Fields{
			"page_id":  pageID,
			"messages": len(messages),
			"feeds":    len(feedEvents),
		}).Debug("Facebook webhook entry processed")

		if len(messages) > 0 {
			go s.dispatchFacebookMessengerEvents(pageID, messages)
		}

		if len(feedEvents) > 0 {
			go s.dispatchFacebookFeedEvents(pageID, feedEvents)
		}
	}

	// Respond immediately — Facebook requires 200 within 20 seconds
	c.String(http.StatusOK, "EVENT_RECEIVED")
}

// dispatchFacebookMessengerEvents routes Messenger messages to matching
// triggers and/or agent channels.
func (s *Service) dispatchFacebookMessengerEvents(pageID string, messages []facebook.MessengerMessage) {
	// Dispatch to flow triggers
	triggerIDs := s.facebookIndex.LookupMessengerTriggers(pageID)
	for _, msg := range messages {
		data := map[string]interface{}{
			"channel_type":    "facebook_messenger",
			"channel_id":      msg.SenderPSID,
			"sender_id":       msg.SenderPSID,
			"message_text":    msg.Text,
			"content":         msg.Text, // Alias for compatibility with AI actions
			"message_id":      msg.MessageID,
			"page_id":         msg.RecipientID,
			"timestamp":       msg.Timestamp,
			"has_attachments": len(msg.Attachments) > 0,
			"is_postback":     msg.IsPostback,
			"postback_title":  msg.PostbackTitle,
			"triggered_at":    time.Now().UTC().Format(time.RFC3339),
		}

		if len(msg.Attachments) > 0 {
			attJSON, _ := json.Marshal(msg.Attachments)
			data["attachments"] = string(attJSON)
		}

		for _, triggerID := range triggerIDs {
			tr, err := s.trigger.GetTriggerByID(triggerID)
			if err != nil || tr == nil {
				continue
			}

			// Carry __node_id from trigger config so the executor injects
			// data into the correct trigger node in multi-trigger flows.
			var triggerCfg map[string]string
			_ = json.Unmarshal(tr.Data, &triggerCfg)
			if nodeID := triggerCfg["__node_id"]; nodeID != "" {
				data["__node_id"] = nodeID
			}

			// Resolve user token → page token so the flow can use it for replies
			pageToken := s.resolveFacebookPageToken(tr, pageID)
			if pageToken != "" {
				data["page_access_token"] = pageToken
			}

			if err := s.trigger.Trigger(tr, data); err != nil {
				log.WithFields(log.Fields{
					"error":      err,
					"trigger_id": triggerID,
					"page_id":    pageID,
				}).Error("failed to fire Facebook Messenger trigger")
			}
		}

		// Dispatch to agent channel
		if agentID, ok := s.facebookIndex.LookupAgent(pageID); ok {
			s.dispatchFacebookMessengerToAgent(agentID, pageID, msg)
		}
	}
}

// dispatchFacebookMessengerToAgent sends a Messenger message to the agent
// service as an inbound message, following the Telegram/Slack pattern.
func (s *Service) dispatchFacebookMessengerToAgent(agentID, pageID string, msg facebook.MessengerMessage) {
	// Get the page access token from the agent's channel config
	pageToken := s.getFacebookPageTokenForAgent(agentID)

	inbound := agent.InboundMessage{
		ChannelType: "facebook_messenger",
		Sender:      msg.SenderPSID,
		Content:     msg.Text,
		Metadata: map[string]interface{}{
			"channel_id":        msg.SenderPSID,
			"user_id":           msg.SenderPSID,
			"message_id":        msg.MessageID,
			"page_id":           pageID,
			"page_access_token": pageToken,
		},
	}

	if err := s.agent.HandleInboundMessage(agentID, inbound); err != nil {
		log.WithFields(log.Fields{
			"error":    err,
			"agent_id": agentID,
			"page_id":  pageID,
			"sender":   msg.SenderPSID,
		}).Error("failed to dispatch Facebook Messenger message to agent")
	}
}

// resolveFacebookPageToken resolves the user access token and app secret
// from a trigger's config, then exchanges for a page access token.
// Returns empty string on failure (non-blocking).
func (s *Service) resolveFacebookPageToken(tr *launch.Trigger, pageID string) string {
	userToken, appSecretVal := s.resolveFacebookCredentials(tr)
	if userToken == "" {
		return ""
	}

	pageToken, err := facebook.GetPageToken(userToken, appSecretVal, pageID)
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"page_id":    pageID,
			"error":      err,
		}).Warn("Facebook: failed to resolve page token for dispatch")
		return ""
	}
	return pageToken
}

// dispatchFacebookFeedEvents routes feed events to matching triggers.
func (s *Service) dispatchFacebookFeedEvents(pageID string, events []facebook.FeedEvent) {
	triggerIDs := s.facebookIndex.LookupFeedTriggers(pageID)

	for _, evt := range events {
		data := map[string]interface{}{
			"channel_type":  "facebook_feed",
			"event_type":    evt.EventType,
			"verb":          evt.Verb,
			"item_id":       evt.ItemID,
			"parent_id":     evt.ParentID,
			"post_id":       evt.PostID,
			"sender_id":     evt.SenderID,
			"sender_name":   evt.SenderName,
			"message":       evt.Message,
			"reaction_type": evt.ReactionType,
			"page_id":       evt.PageID,
			"triggered_at":  time.Now().UTC().Format(time.RFC3339),
		}

		for _, triggerID := range triggerIDs {
			tr, err := s.trigger.GetTriggerByID(triggerID)
			if err != nil || tr == nil {
				continue
			}

			// Carry __node_id from trigger config
			var triggerData map[string]string
			_ = json.Unmarshal(tr.Data, &triggerData)
			if nodeID := triggerData["__node_id"]; nodeID != "" {
				data["__node_id"] = nodeID
			}

			// Apply event filter
			if !facebook.MatchesFilter(evt.EventType, triggerData["event_filter"]) {
				continue
			}

			if err := s.trigger.Trigger(tr, data); err != nil {
				log.WithFields(log.Fields{
					"error":      err,
					"trigger_id": triggerID,
					"page_id":    pageID,
					"event_type": evt.EventType,
				}).Error("failed to fire Facebook feed trigger")
			}
		}
	}
}
