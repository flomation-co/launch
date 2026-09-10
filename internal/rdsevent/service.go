// Package rdsevent is the Launch poller for the RDS Event trigger. It mirrors the
// S3 poller (internal/s3): a lease-guarded 60s watch loop that, per trigger,
// resolves the AWS credentials, calls RDS DescribeEvents over a rolling window,
// and fires the flow once per newly-observed event.
//
// RDS events carry no stable unique ID, so an event is keyed by
// source|type|timestamp|message and tracked in trigger_state exactly like the S3
// poller tracks object keys: a key first seen after the initial baseline fires;
// keys that age out of the DescribeEvents window are pruned (no fire). The
// __initialized sentinel baselines the first poll so pre-existing events in the
// lookback window don't stampede the flow on registration.
package rdsevent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	DefaultPollInterval = 60 * time.Second
	LeaseDuration       = 2 * time.Minute

	// sentinelKey marks that the first (baseline) poll has completed.
	sentinelKey = "__initialized"
)

type rdsEventTriggerConfig struct {
	AwsAccessKey     string `json:"aws_access_key"`
	AwsSecretKey     string `json:"aws_secret_key"`
	Region           string `json:"aws_region"`
	SourceType       string `json:"source_type"`
	SourceIdentifier string `json:"source_identifier"`
	EventCategories  string `json:"event_categories"`
	PollInterval     string `json:"poll_interval"`
}

// eventState is the per-event payload stored in trigger_state and forwarded to the
// flow when the event first fires.
type eventState struct {
	SourceIdentifier string `json:"source_identifier"`
	SourceType       string `json:"source_type"`
	SourceArn        string `json:"source_arn"`
	Message          string `json:"message"`
	EventCategories  string `json:"event_categories"`
	Date             string `json:"date"`
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
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeRDSEvent)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("unable to get rds event triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg rdsEventTriggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse rds event trigger config")
		return
	}
	if cfg.Region == "" {
		log.WithFields(log.Fields{"trigger_id": tr.ID}).Warn("rds event trigger has no region")
		return
	}

	// Only one Launch instance polls each trigger.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to acquire lease for rds event trigger")
		return
	}
	if !acquired {
		return
	}

	// Resolve ${secrets.X} references at poll time; credentials never rest here.
	accessKey := s.trigger.ResolveString(tr.ID, cfg.AwsAccessKey)
	secretKey := s.trigger.ResolveString(tr.ID, cfg.AwsSecretKey)
	region := s.trigger.ResolveString(tr.ID, cfg.Region)
	sourceIdentifier := s.trigger.ResolveString(tr.ID, cfg.SourceIdentifier)

	client, err := createRDSClient(accessKey, secretKey, region)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to create rds client")
		return
	}

	currentEvents, err := s.listEvents(client, cfg, sourceIdentifier)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to list rds events")
		return
	}

	knownState, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to get trigger state")
		return
	}
	_, initialised := knownState[sentinelKey]
	isFirstPoll := !initialised

	for key, ev := range currentEvents {
		if _, exists := knownState[key]; exists {
			delete(knownState, key) // still in window; keep it
			continue
		}
		stateJSON, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if err := s.db.UpsertTriggerState(tr.ID, key, stateJSON); err != nil {
			log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID, "key": key}).Error("unable to upsert trigger state")
			continue
		}
		if !isFirstPoll {
			s.fireTrigger(tr, ev)
		}
	}

	// Remaining known keys (minus sentinel) have aged out of the window — prune.
	for key := range knownState {
		if key == sentinelKey {
			continue
		}
		if err := s.db.DeleteTriggerState(tr.ID, key); err != nil {
			log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID, "key": key}).Error("unable to delete trigger state")
		}
	}

	if isFirstPoll {
		sentinelData, _ := json.Marshal(map[string]string{"status": "initialised"})
		if err := s.db.UpsertTriggerState(tr.ID, sentinelKey, sentinelData); err != nil {
			log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to set initialised sentinel")
		}
	}
}

// listEvents calls DescribeEvents over the default lookback window and returns a
// map keyed by a stable per-event hash.
func (s *Service) listEvents(client *rds.Client, cfg rdsEventTriggerConfig, sourceIdentifier string) (map[string]eventState, error) {
	ctx := context.Background()
	result := make(map[string]eventState)

	input := &rds.DescribeEventsInput{}
	if st := strings.TrimSpace(cfg.SourceType); st != "" {
		input.SourceType = rdstypes.SourceType(st)
	}
	if sourceIdentifier != "" {
		input.SourceIdentifier = aws.String(sourceIdentifier)
	}
	if cats := splitCSV(cfg.EventCategories); len(cats) > 0 {
		input.EventCategories = cats
	}

	paginator := rds.NewDescribeEventsPaginator(client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe events: %w", err)
		}
		for _, ev := range page.Events {
			date := ""
			if ev.Date != nil {
				date = ev.Date.UTC().Format(time.RFC3339)
			}
			es := eventState{
				SourceIdentifier: aws.ToString(ev.SourceIdentifier),
				SourceType:       string(ev.SourceType),
				SourceArn:        aws.ToString(ev.SourceArn),
				Message:          aws.ToString(ev.Message),
				EventCategories:  strings.Join(ev.EventCategories, ","),
				Date:             date,
			}
			result[eventKey(es)] = es
		}
	}
	return result, nil
}

func (s *Service) fireTrigger(tr *launch.Trigger, ev eventState) {
	log.WithFields(log.Fields{
		"trigger_id": tr.ID,
		"source":     ev.SourceIdentifier,
		"type":       ev.SourceType,
	}).Info("rds event detected, firing trigger")

	data := map[string]interface{}{
		"source_identifier": ev.SourceIdentifier,
		"source_type":       ev.SourceType,
		"source_arn":        ev.SourceArn,
		"message":           ev.Message,
		"event_categories":  ev.EventCategories,
		"date":              ev.Date,
		"triggered_at":      time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to fire rds event trigger")
	}
}

// eventKey is a stable identity for an RDS event (which has no ID field), so the
// same event is never fired twice across overlapping poll windows.
func eventKey(ev eventState) string {
	sum := sha256.Sum256([]byte(ev.SourceIdentifier + "|" + ev.SourceType + "|" + ev.Date + "|" + ev.Message))
	return hex.EncodeToString(sum[:])
}

func createRDSClient(accessKey, secretKey, region string) (*rds.Client, error) {
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}
	return rds.NewFromConfig(cfg), nil
}

func splitCSV(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
