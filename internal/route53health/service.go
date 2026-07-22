// Package route53health is the Launch poller for the Route 53 Health Check
// trigger. It mirrors the CloudWatch/RDS-event pollers: a lease-guarded 60s watch
// loop that, per trigger, resolves the AWS credentials at poll time, calls Route 53
// GetHealthCheckStatus, computes an aggregate healthy/unhealthy verdict from the
// per-checker observations, and fires the flow when that verdict flips (matching an
// optional fire_on filter).
//
// Route 53 is global, so no region is needed. The aggregate verdict is a v1
// heuristic — Healthy when a majority of reporting health checkers observe success.
// The first poll baselines the status so a health check that is already unhealthy
// at registration doesn't fire immediately.
package route53health

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/route53"
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

	statusKey = "status"
)

type healthCheckTriggerConfig struct {
	AwsAccessKey  string `json:"aws_access_key"`
	AwsSecretKey  string `json:"aws_secret_key"`
	Region        string `json:"aws_region"`
	HealthCheckID string `json:"health_check_id"`
	FireOn        string `json:"fire_on"`
	PollInterval  string `json:"poll_interval"`
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
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeRoute53HealthCheck)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("unable to get route53 health check triggers")
		return
	}
	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg healthCheckTriggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse route53 health check trigger config")
		return
	}
	if strings.TrimSpace(cfg.HealthCheckID) == "" {
		return
	}

	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil || !acquired {
		return
	}

	accessKey := s.trigger.ResolveString(tr.ID, cfg.AwsAccessKey)
	secretKey := s.trigger.ResolveString(tr.ID, cfg.AwsSecretKey)
	region := s.trigger.ResolveString(tr.ID, cfg.Region)
	if region == "" {
		region = "us-east-1" // Route 53 is global; any region works.
	}

	client, err := createRoute53Client(accessKey, secretKey, region)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to create route53 client")
		return
	}

	out, err := client.GetHealthCheckStatus(context.Background(), &route53.GetHealthCheckStatusInput{
		HealthCheckId: aws.String(cfg.HealthCheckID),
	})
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to get health check status")
		return
	}

	healthy, unhealthy := 0, 0
	for _, obs := range out.HealthCheckObservations {
		if obs.StatusReport == nil {
			continue
		}
		if strings.HasPrefix(aws.ToString(obs.StatusReport.Status), "Success") {
			healthy++
		} else {
			unhealthy++
		}
	}
	total := healthy + unhealthy
	if total == 0 {
		return // no checkers reporting yet — no verdict
	}
	status := "Unhealthy"
	if healthy*2 > total {
		status = "Healthy"
	}

	known, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		return
	}
	previous := ""
	if raw, ok := known[statusKey]; ok {
		var st struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &st)
		previous = st.Status
	}

	stateJSON, _ := json.Marshal(map[string]string{"status": status})
	_ = s.db.UpsertTriggerState(tr.ID, statusKey, stateJSON)

	if previous == "" || previous == status {
		return // first poll (baseline) or no change
	}

	if s.shouldFire(cfg.FireOn, status) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "health_check": cfg.HealthCheckID, "status": status}).Info("route53 health check status change, firing trigger")
		s.dispatch(tr, map[string]interface{}{
			"health_check_id": cfg.HealthCheckID,
			"status":          status,
			"previous_status": previous,
			"healthy_count":   healthy,
			"unhealthy_count": unhealthy,
			"triggered_at":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// shouldFire applies the fire_on filter: unhealthy → fire when the new status is
// Unhealthy; healthy → fire when it recovers; blank → fire on any change.
func (s *Service) shouldFire(fireOn, status string) bool {
	switch strings.TrimSpace(fireOn) {
	case "unhealthy":
		return status == "Unhealthy"
	case "healthy":
		return status == "Healthy"
	default:
		return true
	}
}

func (s *Service) dispatch(tr *launch.Trigger, data map[string]interface{}) {
	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to fire route53 health check trigger")
	}
}

func createRoute53Client(accessKey, secretKey, region string) (*route53.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}
	return route53.NewFromConfig(cfg), nil
}
