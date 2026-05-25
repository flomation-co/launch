// Package linkedin provides a polling trigger service that monitors LinkedIn
// posts for new comments and reactions, dispatching events to flows.
package linkedin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
	log "github.com/sirupsen/logrus"
)

const (
	linkedinRESTBase   = "https://api.linkedin.com/rest"
	linkedinVersion    = "202604"
	defaultPollSeconds = 300 // 5 minutes
	minPollSeconds     = 60
)

type triggerConfig struct {
	AccessToken    string `json:"access_token"`
	OrganizationID string `json:"organization_id"`
	PostURN        string `json:"post_urn"`
	EventFilter    string `json:"event_filter"`
	PollInterval   string `json:"poll_interval"`
}

type pollState struct {
	LastCommentTS int64 `json:"last_comment_ts"`
	LastLikeTS    int64 `json:"last_like_ts"`
}

// Service polls LinkedIn for new activity on monitored posts.
type Service struct {
	config  *config.Config
	db      *persistence.Service
	trigger *trigger.Service
	client  *http.Client
}

// NewService creates and starts the LinkedIn polling service.
func NewService(cfg *config.Config, db *persistence.Service, triggerSvc *trigger.Service) *Service {
	s := &Service{
		config:  cfg,
		db:      db,
		trigger: triggerSvc,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	go s.watch()
	return s
}

func (s *Service) watch() {
	time.Sleep(10 * time.Second)

	log.Info("LinkedIn poll service started")

	for {
		s.poll()
		time.Sleep(60 * time.Second)
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeLinkedInPoll)
	if err != nil {
		log.WithError(err).Warn("linkedin poll: failed to fetch triggers")
		return
	}

	for _, tr := range triggers {
		if tr.DisabledAt != nil {
			continue
		}

		var cfg triggerConfig
		_ = json.Unmarshal(tr.Data, &cfg)

		// Check poll interval
		interval := defaultPollSeconds
		if cfg.PollInterval != "" {
			if n, err := strconv.Atoi(cfg.PollInterval); err == nil && n >= minPollSeconds {
				interval = n
			}
		}

		// Use trigger_state to track last poll time
		stateKey := "linkedin_last_poll"
		allState, _ := s.db.GetTriggerState(tr.ID)
		if raw, ok := allState[stateKey]; ok {
			var lastPoll int64
			_ = json.Unmarshal(raw, &lastPoll)
			if time.Now().Unix()-lastPoll < int64(interval) {
				continue // Not time to poll yet
			}
		}

		s.checkTrigger(tr, &cfg)

		// Update last poll time
		nowBytes, _ := json.Marshal(time.Now().Unix())
		_ = s.db.UpsertTriggerState(tr.ID, stateKey, nowBytes)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger, cfg *triggerConfig) {
	// Resolve access token
	accessToken := cfg.AccessToken
	if strings.Contains(accessToken, "${") {
		resolved, err := s.trigger.ResolveVariables(tr.ID, []string{accessToken})
		if err != nil || resolved[accessToken] == "" {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"error":      err,
			}).Warn("linkedin poll: failed to resolve access token")
			return
		}
		accessToken = resolved[accessToken]
	}

	if accessToken == "" {
		return
	}

	// Load state
	stateKey := "linkedin_state"
	var state pollState
	if allState, err := s.db.GetTriggerState(tr.ID); err == nil {
		if raw, ok := allState[stateKey]; ok {
			_ = json.Unmarshal(raw, &state)
		}
	}

	if cfg.PostURN == "" {
		return // No specific post to monitor
	}

	changed := false
	filter := cfg.EventFilter
	if filter == "" {
		filter = "comment,reaction"
	}

	// Poll comments
	if strings.Contains(filter, "comment") {
		if s.pollComments(tr, accessToken, cfg.PostURN, &state) {
			changed = true
		}
	}

	// Poll reactions
	if strings.Contains(filter, "reaction") {
		if s.pollReactions(tr, accessToken, cfg.PostURN, &state) {
			changed = true
		}
	}

	if changed {
		stateBytes, _ := json.Marshal(state)
		_ = s.db.UpsertTriggerState(tr.ID, stateKey, stateBytes)
	}
}

func (s *Service) pollComments(tr *launch.Trigger, token, postURN string, state *pollState) bool {
	url := fmt.Sprintf("%s/socialActions/%s/comments?count=20", linkedinRESTBase, postURN)

	body, err := s.linkedinGet(url, token)
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("linkedin poll: failed to fetch comments")
		return false
	}

	var resp struct {
		Elements []struct {
			Actor     string                `json:"actor"`
			Message   struct{ Text string } `json:"message"`
			CreatedAt int64                 `json:"created"`
			ID        string                `json:"$URN"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}

	changed := false
	for _, elem := range resp.Elements {
		if elem.CreatedAt <= state.LastCommentTS {
			continue
		}

		data := map[string]interface{}{
			"event_type":   "comment",
			"post_urn":     postURN,
			"author_urn":   elem.Actor,
			"author_name":  "", // Would need profile lookup to resolve
			"content":      elem.Message.Text,
			"comment_urn":  elem.ID,
			"created_at":   time.UnixMilli(elem.CreatedAt).UTC().Format(time.RFC3339),
			"triggered_at": time.Now().UTC().Format(time.RFC3339),
		}

		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithError(err).Warn("linkedin poll: failed to fire comment trigger")
		}

		if elem.CreatedAt > state.LastCommentTS {
			state.LastCommentTS = elem.CreatedAt
			changed = true
		}
	}
	return changed
}

func (s *Service) pollReactions(tr *launch.Trigger, token, postURN string, state *pollState) bool {
	url := fmt.Sprintf("%s/socialActions/%s/likes?count=20", linkedinRESTBase, postURN)

	body, err := s.linkedinGet(url, token)
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("linkedin poll: failed to fetch reactions")
		return false
	}

	var resp struct {
		Elements []struct {
			Actor     string `json:"actor"`
			CreatedAt int64  `json:"created"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}

	changed := false
	for _, elem := range resp.Elements {
		if elem.CreatedAt <= state.LastLikeTS {
			continue
		}

		data := map[string]interface{}{
			"event_type":    "reaction",
			"post_urn":      postURN,
			"author_urn":    elem.Actor,
			"author_name":   "",
			"reaction_type": "like",
			"created_at":    time.UnixMilli(elem.CreatedAt).UTC().Format(time.RFC3339),
			"triggered_at":  time.Now().UTC().Format(time.RFC3339),
		}

		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithError(err).Warn("linkedin poll: failed to fire reaction trigger")
		}

		if elem.CreatedAt > state.LastLikeTS {
			state.LastLikeTS = elem.CreatedAt
			changed = true
		}
	}
	return changed
}

func (s *Service) linkedinGet(url, token string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Restli-Protocol-Version", "2.0.0")
	req.Header.Set("LinkedIn-Version", linkedinVersion)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LinkedIn API returned %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}
	return body, nil
}
