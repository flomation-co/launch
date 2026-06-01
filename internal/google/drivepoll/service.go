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

	// Resolve credential from secrets/environment.
	credential := s.trigger.ResolveString(tr.ID, cfg.Credential)
	if credential == "" {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
		}).Debug("[drive-poll] no credential configured")
		return
	}

	// Get Google Drive access token.
	accessToken, err := s.fetchAccessToken(tr.ID, credential)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Debug("[drive-poll] unable to fetch access token")
		return
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

// fetchAccessToken returns a Drive access token. If the credential is a
// raw access token (already resolved from secrets), it is used directly.
func (s *Service) fetchAccessToken(_ string, credential string) (string, error) {
	// The credential is resolved by ResolveString, which handles
	// ${secrets.X}, ${env.X}, etc. The resulting value is the access
	// token ready to use.
	if credential != "" {
		return credential, nil
	}
	return "", fmt.Errorf("no drive credential available")
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
