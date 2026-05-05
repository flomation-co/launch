package agent

// Background goroutine that generates embeddings for memories written
// without them (via the extraction pipeline or legacy writes). Runs every
// 15 seconds, fetches a batch of unembedded memories from the API,
// generates embeddings via Bedrock, and patches them back.
//
// This decouples embedding generation from the write path, keeping memory
// creation fast and resilient to Bedrock latency or outages. Memories
// become semantically searchable within ~15 seconds of creation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	backfillInterval       = 15 * time.Second
	backfillBatch          = 10
	backfillPerItemTimeout = 10 * time.Second
)

// startEmbeddingBackfill launches the background backfill loop if
// embedding is enabled. Called from NewService.
func (s *Service) startEmbeddingBackfill() {
	if s.embedding == nil {
		return
	}

	go func() {
		// Small initial delay to let the API come up.
		time.Sleep(5 * time.Second)

		ticker := time.NewTicker(backfillInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.backfillBatch()
		}
	}()
}

type unembeddedMemory struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (s *Service) backfillBatch() {
	apiURL := s.config.InternalAPIURL()
	if apiURL == "" {
		return
	}

	// 1. Fetch memories without embeddings.
	endpoint := fmt.Sprintf("%s/api/v1/internal/memory/unembedded?limit=%d", apiURL, backfillBatch)
	resp, err := s.apiClient.Get(endpoint) // #nosec G107 — internal service-to-service call
	if err != nil {
		log.WithError(err).Debug("embedding backfill: failed to fetch unembedded memories")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var memories []unembeddedMemory
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&memories); err != nil {
		return
	}

	if len(memories) == 0 {
		return
	}

	log.WithField("count", len(memories)).Debug("embedding backfill: processing batch")

	// 2. Generate embeddings and patch each one with a per-item timeout.
	for _, mem := range memories {
		text := mem.Title
		if mem.Body != "" {
			if text != "" {
				text += ": "
			}
			text += mem.Body
		}
		if text == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), backfillPerItemTimeout)

		vec, err := s.embedding.Embed(ctx, text)
		if err != nil {
			cancel()
			log.WithFields(log.Fields{
				"memory_id": mem.ID,
				"error":     err,
			}).Warn("embedding backfill: failed to generate embedding")
			continue
		}

		// 3. PATCH the embedding back to the API.
		payload, _ := json.Marshal(map[string]interface{}{
			"embedding": vec,
		})

		patchURL := fmt.Sprintf("%s/api/v1/internal/memory/%s/embedding", apiURL, mem.ID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(payload))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		patchResp, err := s.apiClient.Do(req)
		if err != nil {
			cancel()
			log.WithFields(log.Fields{
				"memory_id": mem.ID,
				"error":     err,
			}).Warn("embedding backfill: failed to patch embedding")
			continue
		}
		_ = patchResp.Body.Close()
		cancel()

		log.WithField("memory_id", mem.ID).Debug("embedding backfill: embedded")
	}
}
