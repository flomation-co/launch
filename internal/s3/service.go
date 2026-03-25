package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	DefaultPollInterval = 60 * time.Second
	MinPollInterval     = 10 * time.Second
	LeaseDuration       = 2 * time.Minute

	// sentinelKey is stored in trigger_state to mark that the first poll has completed.
	sentinelKey = "__initialized"
)

type s3TriggerConfig struct {
	BucketName   string `json:"bucket_name"`
	Prefix       string `json:"prefix"`
	AwsAccessKey string `json:"aws_access_key"`
	AwsSecretKey string `json:"aws_secret_key"`
	Region       string `json:"region"`
	PollInterval string `json:"poll_interval"`
	EventTypes   string `json:"event_types"`
}

// objectState holds the metadata stored per S3 object in trigger_state.
type objectState struct {
	ETag         string `json:"etag"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified"`
}

type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	instanceID string
}

func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service) *Service {
	s := &Service{
		config:     config,
		db:         db,
		trigger:    trigger,
		instanceID: uuid.New().String(),
	}

	go s.watch()

	return s
}

func (s *Service) watch() {
	time.Sleep(5 * time.Second)

	for {
		s.poll()
		time.Sleep(DefaultPollInterval)
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeS3)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to get s3 triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg s3TriggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("unable to parse s3 trigger config")
		return
	}

	if cfg.BucketName == "" {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
		}).Warn("s3 trigger has no bucket name")
		return
	}

	if cfg.Region == "" {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
		}).Warn("s3 trigger has no region")
		return
	}

	// Acquire lease to prevent duplicate polling across instances.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("unable to acquire lease for s3 trigger")
		return
	}
	if !acquired {
		return
	}

	// Resolve variable references in credentials.
	accessKey := s.trigger.ResolveString(tr.ID, cfg.AwsAccessKey)
	secretKey := s.trigger.ResolveString(tr.ID, cfg.AwsSecretKey)
	bucketName := s.trigger.ResolveString(tr.ID, cfg.BucketName)
	prefix := s.trigger.ResolveString(tr.ID, cfg.Prefix)

	// Determine which event types to fire for.
	eventTypes := parseEventTypes(cfg.EventTypes)

	// Create S3 client.
	client, err := createS3Client(accessKey, secretKey, cfg.Region)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("unable to create s3 client")
		return
	}

	// List all objects in the bucket.
	currentObjects, err := listObjects(client, bucketName, prefix)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"bucket":     bucketName,
		}).Error("unable to list s3 objects")
		return
	}

	// Load known state from DB.
	knownState, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("unable to get trigger state")
		return
	}

	// Check if this is the first poll (no sentinel key).
	_, initialised := knownState[sentinelKey]
	isFirstPoll := !initialised

	// Process current objects: detect new and changed.
	for key, obj := range currentObjects {
		stateJSON, err := json.Marshal(obj)
		if err != nil {
			continue
		}

		existingData, exists := knownState[key]
		if !exists {
			// New object — record state.
			if err := s.db.UpsertTriggerState(tr.ID, key, stateJSON); err != nil {
				log.WithFields(log.Fields{
					"error":      err,
					"trigger_id": tr.ID,
					"key":        key,
				}).Error("unable to upsert trigger state")
				continue
			}

			if !isFirstPoll && eventTypes["put"] {
				s.fireTrigger(tr, bucketName, key, obj, "put")
			}
			continue
		}

		// Existing object — check for ETag change.
		var existing objectState
		if err := json.Unmarshal(existingData, &existing); err != nil {
			continue
		}

		if existing.ETag != obj.ETag {
			if err := s.db.UpsertTriggerState(tr.ID, key, stateJSON); err != nil {
				log.WithFields(log.Fields{
					"error":      err,
					"trigger_id": tr.ID,
					"key":        key,
				}).Error("unable to upsert trigger state")
				continue
			}

			if eventTypes["put"] {
				s.fireTrigger(tr, bucketName, key, obj, "put")
			}
		}

		// Remove from knownState so the remainder is deleted objects.
		delete(knownState, key)
	}

	// Remaining keys in knownState (excluding sentinel) are deleted objects.
	for key := range knownState {
		if key == sentinelKey {
			continue
		}

		var deleted objectState
		_ = json.Unmarshal(knownState[key], &deleted)

		if err := s.db.DeleteTriggerState(tr.ID, key); err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": tr.ID,
				"key":        key,
			}).Error("unable to delete trigger state")
			continue
		}

		if !isFirstPoll && eventTypes["delete"] {
			s.fireTrigger(tr, bucketName, key, deleted, "delete")
		}
	}

	// Mark as initialised if first poll.
	if isFirstPoll {
		sentinelData, _ := json.Marshal(map[string]string{"status": "initialised"})
		if err := s.db.UpsertTriggerState(tr.ID, sentinelKey, sentinelData); err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": tr.ID,
			}).Error("unable to set initialised sentinel")
		}
	}
}

func (s *Service) fireTrigger(tr *launch.Trigger, bucket, key string, obj objectState, eventType string) {
	log.WithFields(log.Fields{
		"trigger_id": tr.ID,
		"bucket":     bucket,
		"key":        key,
		"event_type": eventType,
	}).Info("s3 change detected, firing trigger")

	data := map[string]interface{}{
		"bucket":        bucket,
		"key":           key,
		"size":          obj.Size,
		"last_modified": obj.LastModified,
		"etag":          obj.ETag,
		"event_type":    eventType,
	}

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"key":        key,
		}).Error("unable to fire s3 trigger")
	}
}

func createS3Client(accessKey, secretKey, region string) (*s3.Client, error) {
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}

	return s3.NewFromConfig(cfg), nil
}

func listObjects(client *s3.Client, bucket, prefix string) (map[string]objectState, error) {
	ctx := context.Background()
	result := make(map[string]objectState)

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	paginator := s3.NewListObjectsV2Paginator(client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("unable to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			result[key] = objectState{
				ETag:         aws.ToString(obj.ETag),
				Size:         aws.ToInt64(obj.Size),
				LastModified: obj.LastModified.Format(time.RFC3339),
			}
		}
	}

	return result, nil
}

func parseEventTypes(raw string) map[string]bool {
	result := map[string]bool{
		"put":    true,
		"delete": true,
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return result
	}

	result = map[string]bool{}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "put" || t == "delete" {
			result[t] = true
		}
	}

	return result
}
