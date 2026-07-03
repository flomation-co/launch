package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/facebook"

	log "github.com/sirupsen/logrus"

	"github.com/gin-gonic/gin"
)

func (s *Service) createTrigger(c *gin.Context) {
	id := c.Param("id")

	t, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to get trigger by ID")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var tr launch.Trigger
	if err := c.BindJSON(&tr); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to bind json")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Use the API's trigger ID so upsert works correctly
	tr.ID = id

	r, err := s.trigger.CreateTrigger(tr)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to upsert trigger")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Update Facebook page index for Facebook triggers
	s.updateFacebookIndex(&tr)

	// Telegram triggers: programmatically set the Bot API webhook URL to
	// /webhook/telegram/{trigger_id}. The handler also accepts the
	// legacy /webhook/telegram/{agent_id} URL via fallback, but new
	// trigger-keyed registrations route through the unified dispatch.
	s.registerTelegramTriggerWebhook(&tr)

	// Mailchimp triggers: auto-register an audience webhook pointing at
	// /webhook/{trigger_id} (idempotent). Errors are logged, not fatal.
	s.registerMailchimpWebhook(&tr)

	// Calendly triggers: auto-register a webhook subscription pointing at
	// /webhook/{trigger_id} (idempotent). Errors are logged, not fatal.
	s.registerCalendlyWebhook(&tr)

	// Zendesk triggers: auto-register a webhook connector + business rule
	// pointing at /webhook/{trigger_id} (idempotent). Errors are logged, not fatal.
	s.registerZendeskWebhook(&tr)

	// Cal.com triggers: auto-register a webhook pointing at
	// /webhook/{trigger_id} (idempotent). Errors are logged, not fatal.
	s.registerCalcomWebhook(&tr)

	// Acuity triggers: auto-register one webhook per selected event pointing at
	// /webhook/{trigger_id} (idempotent). Errors are logged, not fatal.
	s.registerAcuityWebhook(&tr)

	if t == nil {
		c.JSON(http.StatusCreated, r)
	} else {
		c.JSON(http.StatusOK, r)
	}
}

// registerTelegramTriggerWebhook calls Telegram's setWebhook API for a
// telegram-type trigger, pointing at /webhook/telegram/{trigger_id}.
// Errors are logged but don't fail the trigger upsert — the agent
// service may also register the webhook from its side for agent-owned
// triggers, and the user can hit /api/v1/internal/trigger/:id again to
// retry.
func (s *Service) registerTelegramTriggerWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeTelegram || s.telegram == nil {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	botToken := creds["bot_token"]
	if botToken == "" {
		log.WithField("trigger_id", tr.ID).Warn("telegram trigger upsert: no bot_token resolved; skipping webhook registration")
		return
	}
	if err := s.telegram.RegisterWebhook(tr.ID, botToken); err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("failed to register Telegram webhook for trigger")
	}
}

func (s *Service) deleteTrigger(c *gin.Context) {
	id := c.Param("id")

	t, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to get trigger by ID")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if t == nil {
		// Already deleted or never existed — treat as success
		c.Status(http.StatusOK)
		return
	}

	// Remove from Facebook page index before deleting
	s.facebookIndex.RemoveTrigger(t.ID)

	// Deregister Telegram webhook if this is a telegram trigger we
	// previously registered.
	if t.Type == launch.TriggerTypeTelegram && s.telegram != nil {
		if err := s.telegram.DeregisterWebhook(t.ID); err != nil {
			log.WithFields(log.Fields{
				"trigger_id": t.ID,
				"error":      err,
			}).Warn("failed to deregister Telegram webhook for trigger")
		}
	}

	// Deregister the Mailchimp audience webhook we previously created.
	if t.Type == launch.TriggerTypeMailchimpWebhook {
		s.deregisterMailchimpWebhook(t)
	}

	// Deregister the Calendly webhook subscription we previously created.
	if t.Type == launch.TriggerTypeCalendlyWebhook {
		s.deregisterCalendlyWebhook(t)
	}

	// Deregister the Zendesk webhook connector + business rule we created.
	if t.Type == launch.TriggerTypeZendeskWebhook {
		s.deregisterZendeskWebhook(t)
	}

	// Deregister the Cal.com webhook we created.
	if t.Type == launch.TriggerTypeCalcomWebhook {
		s.deregisterCalcomWebhook(t)
	}

	// Deregister the Acuity webhook subscriptions we created.
	if t.Type == launch.TriggerTypeAcuityWebhook {
		s.deregisterAcuityWebhook(t)
	}

	if err := s.trigger.RemoveTrigger(*t); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to delete trigger")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.Status(http.StatusOK)
}

// updateFacebookIndex adds a Facebook trigger to the page index and
// auto-subscribes the page to the app's webhook events.
func (s *Service) updateFacebookIndex(tr *launch.Trigger) {
	if tr.Type != launch.TriggerTypeFacebookMessenger && tr.Type != launch.TriggerTypeFacebookFeed {
		return
	}

	var data map[string]string
	_ = json.Unmarshal(tr.Data, &data)
	pageID := data["page_id"]
	if pageID == "" {
		return
	}

	switch tr.Type {
	case launch.TriggerTypeFacebookMessenger:
		s.facebookIndex.AddMessengerTrigger(pageID, tr.ID)
	case launch.TriggerTypeFacebookFeed:
		s.facebookIndex.AddFeedTrigger(pageID, tr.ID)
	}

	log.WithFields(log.Fields{
		"trigger_id": tr.ID,
		"page_id":    pageID,
		"type":       tr.Type,
	}).Info("Facebook trigger registered in page index")

	// Resolve user token → page token and subscribe the page
	go s.subscribeFacebookPage(tr, pageID)
}

// subscribeFacebookPage resolves the user access token from the trigger config,
// exchanges it for a page token, and subscribes the page to webhook events.
func (s *Service) subscribeFacebookPage(tr *launch.Trigger, pageID string) {
	userToken, appSecretVal := s.resolveFacebookCredentials(tr)
	if userToken == "" {
		return
	}

	// Exchange user token for page token
	pageToken, err := facebook.GetPageToken(userToken, appSecretVal, pageID)
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"page_id":    pageID,
			"error":      err,
		}).Warn("Facebook: failed to get page token from user token")
		return
	}

	// Subscribe the page to webhook events
	var fields []string
	switch tr.Type {
	case launch.TriggerTypeFacebookMessenger:
		fields = []string{"messages", "messaging_postbacks"}
	case launch.TriggerTypeFacebookFeed:
		fields = []string{"feed"}
	}

	if err := facebook.SubscribePageToApp(pageToken, appSecretVal, pageID, fields); err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"page_id":    pageID,
			"error":      err,
		}).Warn("Facebook: failed to subscribe page to app")
		return
	}

	log.WithFields(log.Fields{
		"trigger_id": tr.ID,
		"page_id":    pageID,
		"fields":     fields,
	}).Info("Facebook page subscribed to app webhook events")
}

// rebuildFacebookIndex loads all facebook-messenger and facebook-feed triggers
// from the database and rebuilds the in-memory page index. Called on startup.
func (s *Service) rebuildFacebookIndex() {
	for _, triggerType := range []string{launch.TriggerTypeFacebookMessenger, launch.TriggerTypeFacebookFeed} {
		triggers, err := s.db.GetTriggersByType(triggerType)
		if err != nil {
			log.WithError(err).Warn("unable to load Facebook triggers for index rebuild")
			continue
		}
		for _, tr := range triggers {
			if tr.DisabledAt != nil {
				continue
			}
			var data map[string]string
			_ = json.Unmarshal(tr.Data, &data)
			pageID := data["page_id"]
			if pageID == "" {
				continue
			}
			switch triggerType {
			case launch.TriggerTypeFacebookMessenger:
				s.facebookIndex.AddMessengerTrigger(pageID, tr.ID)
			case launch.TriggerTypeFacebookFeed:
				s.facebookIndex.AddFeedTrigger(pageID, tr.ID)
			}
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"page_id":    pageID,
				"type":       triggerType,
			}).Info("Facebook trigger loaded into page index")
		}
	}
}

// resolveFacebookCredentials resolves the user access token and app secret
// from a Facebook trigger's config, handling ${...} variable references.
func (s *Service) resolveFacebookCredentials(tr *launch.Trigger) (userToken, appSecret string) {
	var data map[string]string
	_ = json.Unmarshal(tr.Data, &data)

	userToken = data["access_token"]
	appSecret = data["app_secret"]

	// Collect all variables that need resolving
	var toResolve []string
	if strings.Contains(userToken, "${") {
		toResolve = append(toResolve, userToken)
	}
	if strings.Contains(appSecret, "${") {
		toResolve = append(toResolve, appSecret)
	}

	if len(toResolve) > 0 {
		resolved, err := s.trigger.ResolveVariables(tr.ID, toResolve)
		if err != nil {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"error":      err,
			}).Warn("Facebook: failed to resolve credentials")
			return "", ""
		}
		if v, ok := resolved[userToken]; ok && v != "" {
			userToken = v
		}
		if v, ok := resolved[appSecret]; ok && v != "" {
			appSecret = v
		}
	}

	return userToken, appSecret
}
