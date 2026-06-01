// Package drivepoll polls Google Drive folders for file changes and fires
// triggers when files are created, modified, or deleted. Follows the same
// polling + state tracking pattern as the S3 trigger service.
package drivepoll

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/google"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	DefaultPollInterval = 60 * time.Second
	LeaseDuration       = 2 * time.Minute
	sentinelKey         = "__initialized"
	driveAPI            = "https://www.googleapis.com/drive/v3"
)

// triggerConfig holds the configuration stored in trigger.Data.
type triggerConfig struct {
	FolderID       string `json:"folder_id"`
	Credential     string `json:"credential"`
	GoogleAccount  string `json:"google_account"`
	PollInterval   string `json:"poll_interval"`
	EventTypes     string `json:"event_types"`
	MIMETypeFilter string `json:"mime_type_filter"`
}

// fileState holds metadata stored per file in trigger_state.
type fileState struct {
	Name         string `json:"name"`
	MIMEType     string `json:"mime_type"`
	ModifiedTime string `json:"modified_time"`
	Size         string `json:"size"`
}

// Service polls Google Drive for file changes.
type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	google     *google.Service
	instanceID string
}

// NewService creates a Drive polling service and starts the background goroutine.
func NewService(cfg *config.Config, db *persistence.Service, triggerSvc *trigger.Service, googleSvc *google.Service) *Service {
	s := &Service{
		config:     cfg,
		db:         db,
		trigger:    triggerSvc,
		google:     googleSvc,
		instanceID: uuid.New().String(),
	}

	go s.watch()

	return s
}

func (s *Service) watch() {
	time.Sleep(10 * time.Second) // Initial delay to let other services start

	for {
		s.poll()
		time.Sleep(DefaultPollInterval)
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeGoogleDrive)
	if err != nil {
		log.WithError(err).Error("[drive-poll] unable to get triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg triggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[drive-poll] unable to parse trigger config")
		return
	}

	// Acquire lease to prevent duplicate polling across instances.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[drive-poll] unable to acquire lease")
		return
	}
	if !acquired {
		return
	}

	// Resolve access token: try credential input first, then internal token endpoint.
	var accessToken string

	if cfg.Credential != "" {
		resolved := s.trigger.ResolveString(tr.ID, cfg.Credential)
		if resolved != "" && resolved != cfg.Credential {
			accessToken = resolved
		}
	}

	if accessToken == "" {
		// Fall back to trigger-scoped Google OAuth tokens
		token, err := s.fetchTokenFromEndpoint(tr.ID, cfg.GoogleAccount)
		if err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": tr.ID,
			}).Warn("[drive-poll] unable to fetch access token")
			return
		}
		accessToken = token
	}

	folderID := cfg.FolderID
	if folderID == "" {
		folderID = "root"
	}

	eventTypes := parseEventTypes(cfg.EventTypes)

	// List files in the folder.
	files, err := s.listFiles(accessToken, folderID, cfg.MIMETypeFilter)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"folder_id":  folderID,
		}).Error("[drive-poll] unable to list files")
		return
	}

	// Load known state from DB.
	knownState, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("[drive-poll] unable to get trigger state")
		return
	}

	_, initialised := knownState[sentinelKey]
	isFirstPoll := !initialised

	// Process current files: detect new and modified.
	for fileID, file := range files {
		stateJSON, err := json.Marshal(file)
		if err != nil {
			continue
		}

		existingData, exists := knownState[fileID]
		if !exists {
			// New file — record state.
			if err := s.db.UpsertTriggerState(tr.ID, fileID, stateJSON); err != nil {
				log.WithFields(log.Fields{
					"error":      err,
					"trigger_id": tr.ID,
					"file_id":    fileID,
				}).Error("[drive-poll] unable to upsert trigger state")
				continue
			}

			if !isFirstPoll && eventTypes["new"] {
				s.fireTrigger(tr, fileID, file, folderID, "new")
			}
			continue
		}

		// Existing file — check for modification.
		var existing fileState
		if err := json.Unmarshal(existingData, &existing); err != nil {
			continue
		}

		if existing.ModifiedTime != file.ModifiedTime {
			if err := s.db.UpsertTriggerState(tr.ID, fileID, stateJSON); err != nil {
				continue
			}

			if eventTypes["modified"] {
				s.fireTrigger(tr, fileID, file, folderID, "modified")
			}
		}

		delete(knownState, fileID)
	}

	// Remaining keys are deleted files.
	for fileID := range knownState {
		if fileID == sentinelKey {
			continue
		}

		var deleted fileState
		_ = json.Unmarshal(knownState[fileID], &deleted)

		if err := s.db.DeleteTriggerState(tr.ID, fileID); err != nil {
			continue
		}

		if !isFirstPoll && eventTypes["deleted"] {
			s.fireTrigger(tr, fileID, deleted, folderID, "deleted")
		}
	}

	// Mark as initialised on first poll.
	if isFirstPoll {
		sentinelData, _ := json.Marshal(map[string]string{"status": "initialised"})
		_ = s.db.UpsertTriggerState(tr.ID, sentinelKey, sentinelData)

		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"folder_id":  folderID,
			"file_count": len(files),
		}).Info("[drive-poll] initialised — recorded existing files")
	}
}

func (s *Service) fireTrigger(tr *launch.Trigger, fileID string, file fileState, folderID, eventType string) {
	log.WithFields(log.Fields{
		"trigger_id": tr.ID,
		"file_id":    fileID,
		"file_name":  file.Name,
		"event_type": eventType,
	}).Info("[drive-poll] file change detected, firing trigger")

	data := map[string]interface{}{
		"file_id":       fileID,
		"name":          file.Name,
		"mime_type":     file.MIMEType,
		"size":          file.Size,
		"modified_time": file.ModifiedTime,
		"event_type":    eventType,
		"folder_id":     folderID,
		"web_link":      fmt.Sprintf("https://drive.google.com/file/d/%s/view", fileID),
	}

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"file_id":    fileID,
		}).Error("[drive-poll] unable to fire trigger")
	}
}

// listFiles queries the Drive API for files in a folder.
func (s *Service) listFiles(accessToken, folderID, mimeFilter string) (map[string]fileState, error) {
	qParts := []string{
		fmt.Sprintf("'%s' in parents", folderID),
		"trashed = false",
	}
	if mimeFilter != "" {
		qParts = append(qParts, fmt.Sprintf("mimeType = '%s'", mimeFilter))
	}

	params := url.Values{}
	params.Set("q", strings.Join(qParts, " and "))
	params.Set("pageSize", "1000")
	params.Set("fields", "files(id,name,mimeType,size,modifiedTime)")
	params.Set("orderBy", "modifiedTime desc")

	endpoint := fmt.Sprintf("%s/files?%s", driveAPI, params.Encode())

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("drive API returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MIMEType     string `json:"mimeType"`
			Size         string `json:"size"`
			ModifiedTime string `json:"modifiedTime"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	files := make(map[string]fileState, len(result.Files))
	for _, f := range result.Files {
		files[f.ID] = fileState{
			Name:         f.Name,
			MIMEType:     f.MIMEType,
			ModifiedTime: f.ModifiedTime,
			Size:         f.Size,
		}
	}
	return files, nil
}

// fetchTokenFromEndpoint calls Launch's internal Google token endpoint to get
// an access token for the trigger's connected Google account.
func (s *Service) fetchTokenFromEndpoint(triggerID, accountFilter string) (string, error) {
	if s.google == nil {
		return "", fmt.Errorf("google OAuth not configured")
	}

	port := s.config.HttpListenConfig.Port
	if port == 0 {
		port = 8080
	}
	endpoint := fmt.Sprintf("http://localhost:%d/internal/google/tokens/trigger/%s?purpose=drive", port, triggerID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(endpoint) // #nosec G107
	if err != nil {
		return "", fmt.Errorf("fetch tokens: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokens []struct {
		Email       string `json:"email"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return "", fmt.Errorf("parse tokens: %w", err)
	}

	// If a specific account was requested, find it
	accountFilter = strings.TrimSpace(accountFilter)
	for _, t := range tokens {
		if t.AccessToken == "" || t.Error != "" {
			continue
		}
		if accountFilter != "" && !strings.EqualFold(t.Email, accountFilter) {
			continue
		}
		return t.AccessToken, nil
	}

	return "", fmt.Errorf("no drive tokens available for trigger %s", triggerID)
}

func parseEventTypes(s string) map[string]bool {
	types := map[string]bool{
		"new":      false,
		"modified": false,
		"deleted":  false,
	}
	if s == "" {
		// Default to new and modified.
		types["new"] = true
		types["modified"] = true
		return types
	}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if _, ok := types[t]; ok {
			types[t] = true
		}
	}
	return types
}
