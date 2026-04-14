// Package retention implements the Phase 6 memory retention poller.
//
// Every hour, the poller:
//  1. Fetches agents with a memory_retention_days policy.
//  2. For each, deletes non-pinned memories older than the retention period.
//  3. Deletes any memories past their individual valid_until or expires_at.
//  4. Writes audit log entries for each sweep.
//
// The poller follows the same pattern as the commitment poller: a single
// goroutine with a ticker, fire-and-forget per agent, fail-open on
// individual errors.
package retention

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"flomation.app/automate/launch/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	pollInterval = 1 * time.Hour
	startupDelay = 30 * time.Second
	httpTimeout  = 30 * time.Second
)

// Service is the retention poller loop.
type Service struct {
	config *config.Config
	client *http.Client
}

// NewService creates and starts the retention poller.
func NewService(cfg *config.Config) *Service {
	s := &Service{
		config: cfg,
		client: &http.Client{Timeout: httpTimeout},
	}
	go s.watch()
	return s
}

func (s *Service) watch() {
	time.Sleep(startupDelay)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	log.Info("retention poller started (1h interval)")

	// Run once immediately after startup delay.
	s.poll()

	for range ticker.C {
		s.poll()
	}
}

type retentionPolicy struct {
	ID                  string `json:"id"`
	MemoryRetentionDays int    `json:"memory_retention_days"`
}

func (s *Service) poll() {
	// Step 1: delete individually expired memories (valid_until/expires_at).
	s.deleteExpired()

	// Step 2: enforce per-agent retention policies.
	policies := s.fetchRetentionPolicies()
	if len(policies) == 0 {
		return
	}

	log.WithField("agents", len(policies)).Debug("enforcing retention policies")

	for _, p := range policies {
		s.enforcePolicy(p)
	}
}

func (s *Service) deleteExpired() {
	body, _ := json.Marshal(map[string]interface{}{
		"limit": 500,
	})

	url := fmt.Sprintf("%s/api/v1/internal/memory/bulk-delete", s.config.Automate.URL)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.WithError(err).Warn("retention: failed to delete expired memories")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		var result struct {
			Deleted int64 `json:"deleted"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Deleted > 0 {
			log.WithField("count", result.Deleted).Info("retention: deleted expired memories")
			s.writeAuditLog("", "retention_sweep", result.Deleted)
		}
	}
}

func (s *Service) enforcePolicy(p retentionPolicy) {
	olderThan := time.Now().Add(-time.Duration(p.MemoryRetentionDays) * 24 * time.Hour)

	body, _ := json.Marshal(map[string]interface{}{
		"agent_id":       p.ID,
		"older_than":     olderThan.Format(time.RFC3339),
		"exclude_pinned": true,
	})

	url := fmt.Sprintf("%s/api/v1/internal/memory/bulk-delete", s.config.Automate.URL)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id": p.ID,
			"error":    err,
		}).Warn("retention: failed to enforce policy")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		var result struct {
			Deleted int64 `json:"deleted"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Deleted > 0 {
			log.WithFields(log.Fields{
				"agent_id":       p.ID,
				"retention_days": p.MemoryRetentionDays,
				"deleted":        result.Deleted,
			}).Info("retention: enforced policy")
			s.writeAuditLog(p.ID, "retention_sweep", result.Deleted)
		}
	}
}

func (s *Service) fetchRetentionPolicies() []retentionPolicy {
	url := fmt.Sprintf("%s/api/v1/internal/agent/retention-policies", s.config.Automate.URL)
	resp, err := s.client.Get(url)
	if err != nil {
		log.WithError(err).Warn("retention: failed to fetch policies")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var policies []retentionPolicy
	if err := json.NewDecoder(resp.Body).Decode(&policies); err != nil {
		return nil
	}
	return policies
}

func (s *Service) writeAuditLog(agentID, eventType string, count int64) {
	detail, _ := json.Marshal(map[string]interface{}{
		"deleted_count": count,
	})

	entry := map[string]interface{}{
		"agent_id":      agentID,
		"actor_type":    "retention",
		"actor_id":      "retention_poller",
		"event_type":    eventType,
		"resource_type": "memory",
		"detail":        json.RawMessage(detail),
	}

	body, _ := json.Marshal(entry)
	url := fmt.Sprintf("%s/api/v1/internal/audit-log", s.config.Automate.URL)

	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
}
