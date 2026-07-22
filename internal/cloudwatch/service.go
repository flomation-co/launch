// Package cloudwatch is the Launch poller for the three CloudWatch triggers:
// alarm state-change, metric threshold, and log-pattern match. It mirrors the RDS
// event poller (internal/rdsevent): a lease-guarded 60s watch loop that, per
// trigger, resolves the AWS credentials at poll time (never at rest), calls the
// relevant CloudWatch API, and fires the flow on the appropriate condition.
//
//   - alarm  — DescribeAlarms; fire when an alarm's StateValue changes (optionally
//     filtered to a target state). Per-alarm state is tracked in trigger_state.
//   - metric — GetMetricStatistics; fire when the latest datapoint *enters* breach
//     of the configured threshold (edge-triggered, one key of trigger_state).
//   - logs   — FilterLogEvents over a rolling window from a stored timestamp
//     watermark; fire once per newly-observed matching event.
//
// Every trigger uses a __initialized sentinel to baseline the first poll so
// pre-existing state doesn't stampede the flow on registration.
package cloudwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
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

	sentinelKey  = "__initialized"
	watermarkKey = "watermark"
	breachKey    = "breach_state"

	// maxLogEventsPerPoll caps how many matching log events a single poll will fire
	// so a noisy log group can't stampede the flow.
	maxLogEventsPerPoll = 100
)

type baseConfig struct {
	AwsAccessKey string `json:"aws_access_key"`
	AwsSecretKey string `json:"aws_secret_key"`
	Region       string `json:"aws_region"`
	PollInterval string `json:"poll_interval"`
}

type alarmConfig struct {
	baseConfig
	AlarmNames string `json:"alarm_names"`
	AlarmState string `json:"alarm_state"`
}

type metricConfig struct {
	baseConfig
	Namespace  string `json:"namespace"`
	MetricName string `json:"metric_name"`
	Dimensions string `json:"dimensions"`
	Statistic  string `json:"statistic"`
	Period     string `json:"period"`
	Comparison string `json:"comparison"`
	Threshold  string `json:"threshold"`
}

type logsConfig struct {
	baseConfig
	LogGroupName  string `json:"log_group_name"`
	FilterPattern string `json:"filter_pattern"`
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
	for _, tr := range s.getTriggers(launch.TriggerTypeCloudWatchAlarm) {
		s.checkAlarmTrigger(tr)
	}
	for _, tr := range s.getTriggers(launch.TriggerTypeCloudWatchMetric) {
		s.checkMetricTrigger(tr)
	}
	for _, tr := range s.getTriggers(launch.TriggerTypeCloudWatchLogs) {
		s.checkLogsTrigger(tr)
	}
}

func (s *Service) getTriggers(triggerType string) []*launch.Trigger {
	triggers, err := s.db.GetTriggersByType(triggerType)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "type": triggerType}).Error("unable to get cloudwatch triggers")
		return nil
	}
	return triggers
}

// ---------------------------------------------------------------------------
// Alarm state-change
// ---------------------------------------------------------------------------

// alarmState is the per-alarm value stored in trigger_state.
type alarmState struct {
	StateValue string `json:"state_value"`
	Timestamp  string `json:"timestamp"`
	Reason     string `json:"reason"`
	MetricName string `json:"metric_name"`
	Namespace  string `json:"namespace"`
}

func (s *Service) checkAlarmTrigger(tr *launch.Trigger) {
	var cfg alarmConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse cloudwatch alarm trigger config")
		return
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return
	}
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil || !acquired {
		return
	}

	accessKey := s.trigger.ResolveString(tr.ID, cfg.AwsAccessKey)
	secretKey := s.trigger.ResolveString(tr.ID, cfg.AwsSecretKey)
	region := s.trigger.ResolveString(tr.ID, cfg.Region)

	client, err := createCloudWatchClient(accessKey, secretKey, region)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to create cloudwatch client")
		return
	}

	in := &cloudwatch.DescribeAlarmsInput{AlarmTypes: []cwtypes.AlarmType{cwtypes.AlarmTypeMetricAlarm, cwtypes.AlarmTypeCompositeAlarm}}
	if names := splitCSV(cfg.AlarmNames); len(names) > 0 {
		in.AlarmNames = names
	}

	current := map[string]alarmState{}
	paginator := cloudwatch.NewDescribeAlarmsPaginator(client, in)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to describe alarms")
			return
		}
		for _, a := range page.MetricAlarms {
			current[aws.ToString(a.AlarmName)] = alarmState{
				StateValue: string(a.StateValue),
				Timestamp:  formatTime(a.StateUpdatedTimestamp),
				Reason:     aws.ToString(a.StateReason),
				MetricName: aws.ToString(a.MetricName),
				Namespace:  aws.ToString(a.Namespace),
			}
		}
		for _, a := range page.CompositeAlarms {
			current[aws.ToString(a.AlarmName)] = alarmState{
				StateValue: string(a.StateValue),
				Timestamp:  formatTime(a.StateUpdatedTimestamp),
				Reason:     aws.ToString(a.StateReason),
			}
		}
	}

	known, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to get trigger state")
		return
	}
	_, initialised := known[sentinelKey]
	isFirstPoll := !initialised
	stateFilter := strings.TrimSpace(cfg.AlarmState)

	for name, cur := range current {
		var prev alarmState
		hadPrev := false
		if raw, ok := known[name]; ok {
			_ = json.Unmarshal(raw, &prev)
			hadPrev = true
		}
		changed := !hadPrev || prev.StateValue != cur.StateValue
		if changed {
			stateJSON, _ := json.Marshal(cur)
			_ = s.db.UpsertTriggerState(tr.ID, name, stateJSON)
			if !isFirstPoll && (stateFilter == "" || cur.StateValue == stateFilter) {
				s.fireAlarm(tr, name, cur, prev.StateValue)
			}
		}
	}

	if isFirstPoll {
		sentinel, _ := json.Marshal(map[string]string{"status": "initialised"})
		_ = s.db.UpsertTriggerState(tr.ID, sentinelKey, sentinel)
	}
}

func (s *Service) fireAlarm(tr *launch.Trigger, name string, cur alarmState, previous string) {
	log.WithFields(log.Fields{"trigger_id": tr.ID, "alarm": name, "state": cur.StateValue}).Info("cloudwatch alarm state change, firing trigger")
	s.dispatch(tr, map[string]interface{}{
		"alarm_name":     name,
		"state_value":    cur.StateValue,
		"previous_state": previous,
		"state_reason":   cur.Reason,
		"metric_name":    cur.MetricName,
		"namespace":      cur.Namespace,
		"timestamp":      cur.Timestamp,
		"triggered_at":   time.Now().UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// Metric threshold (edge-triggered)
// ---------------------------------------------------------------------------

func (s *Service) checkMetricTrigger(tr *launch.Trigger) {
	var cfg metricConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse cloudwatch metric trigger config")
		return
	}
	if strings.TrimSpace(cfg.Region) == "" || strings.TrimSpace(cfg.Namespace) == "" || strings.TrimSpace(cfg.MetricName) == "" {
		return
	}
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil || !acquired {
		return
	}

	threshold, err := strconv.ParseFloat(strings.TrimSpace(cfg.Threshold), 64)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID}).Warn("cloudwatch metric trigger has invalid threshold")
		return
	}
	period := int32(300)
	if p, err := strconv.Atoi(strings.TrimSpace(cfg.Period)); err == nil && p > 0 {
		period = int32(p)
	}
	statistic := cfg.Statistic
	if statistic == "" {
		statistic = "Average"
	}

	accessKey := s.trigger.ResolveString(tr.ID, cfg.AwsAccessKey)
	secretKey := s.trigger.ResolveString(tr.ID, cfg.AwsSecretKey)
	region := s.trigger.ResolveString(tr.ID, cfg.Region)

	client, err := createCloudWatchClient(accessKey, secretKey, region)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to create cloudwatch client")
		return
	}

	end := time.Now().UTC()
	start := end.Add(-time.Duration(period) * time.Second * 3)
	out, err := client.GetMetricStatistics(context.Background(), &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(cfg.Namespace),
		MetricName: aws.String(cfg.MetricName),
		Dimensions: parseDimensions(cfg.Dimensions),
		StartTime:  aws.Time(start),
		EndTime:    aws.Time(end),
		Period:     aws.Int32(period),
		Statistics: []cwtypes.Statistic{cwtypes.Statistic(statistic)},
	})
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to get metric statistics")
		return
	}
	if len(out.Datapoints) == 0 {
		return // no data this window — no state change
	}

	// Latest datapoint by timestamp.
	latest := out.Datapoints[0]
	for _, d := range out.Datapoints {
		if d.Timestamp != nil && latest.Timestamp != nil && d.Timestamp.After(*latest.Timestamp) {
			latest = d
		}
	}
	value := statValue(latest, statistic)
	breached := compare(value, threshold, cfg.Comparison)

	known, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		return
	}
	_, initialised := known[sentinelKey]
	wasBreached := false
	if raw, ok := known[breachKey]; ok {
		var st struct {
			Breached bool `json:"breached"`
		}
		_ = json.Unmarshal(raw, &st)
		wasBreached = st.Breached
	}

	stateJSON, _ := json.Marshal(map[string]bool{"breached": breached})
	_ = s.db.UpsertTriggerState(tr.ID, breachKey, stateJSON)
	if !initialised {
		sentinel, _ := json.Marshal(map[string]string{"status": "initialised"})
		_ = s.db.UpsertTriggerState(tr.ID, sentinelKey, sentinel)
		return // baseline only
	}

	// Edge-trigger: fire only on the transition into breach.
	if breached && !wasBreached {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "metric": cfg.MetricName, "value": value}).Info("cloudwatch metric entered breach, firing trigger")
		s.dispatch(tr, map[string]interface{}{
			"namespace":    cfg.Namespace,
			"metric_name":  cfg.MetricName,
			"value":        strconv.FormatFloat(value, 'f', -1, 64),
			"threshold":    cfg.Threshold,
			"comparison":   cfg.Comparison,
			"statistic":    statistic,
			"timestamp":    formatTime(latest.Timestamp),
			"triggered_at": time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// ---------------------------------------------------------------------------
// Logs pattern match
// ---------------------------------------------------------------------------

func (s *Service) checkLogsTrigger(tr *launch.Trigger) {
	var cfg logsConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse cloudwatch logs trigger config")
		return
	}
	if strings.TrimSpace(cfg.Region) == "" || strings.TrimSpace(cfg.LogGroupName) == "" {
		return
	}
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil || !acquired {
		return
	}

	accessKey := s.trigger.ResolveString(tr.ID, cfg.AwsAccessKey)
	secretKey := s.trigger.ResolveString(tr.ID, cfg.AwsSecretKey)
	region := s.trigger.ResolveString(tr.ID, cfg.Region)

	client, err := createLogsClient(accessKey, secretKey, region)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to create cloudwatch logs client")
		return
	}

	known, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		return
	}
	nowMs := time.Now().UTC().UnixMilli()

	// First poll baselines the watermark to now so pre-existing logs don't fire.
	if raw, ok := known[watermarkKey]; !ok {
		wm, _ := json.Marshal(map[string]int64{"ts": nowMs})
		_ = s.db.UpsertTriggerState(tr.ID, watermarkKey, wm)
		return
	} else {
		var st struct {
			TS int64 `json:"ts"`
		}
		_ = json.Unmarshal(raw, &st)
		s.pollLogEvents(tr, client, cfg, st.TS)
	}
}

func (s *Service) pollLogEvents(tr *launch.Trigger, client *cloudwatchlogs.Client, cfg logsConfig, sinceMs int64) {
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(cfg.LogGroupName),
		StartTime:    aws.Int64(sinceMs + 1),
	}
	if p := strings.TrimSpace(cfg.FilterPattern); p != "" {
		in.FilterPattern = aws.String(p)
	}

	maxTS := sinceMs
	fired := 0
	paginator := cloudwatchlogs.NewFilterLogEventsPaginator(client, in)
	for paginator.HasMorePages() && fired < maxLogEventsPerPoll {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to filter log events")
			return
		}
		for _, ev := range page.Events {
			ts := aws.ToInt64(ev.Timestamp)
			if ts > maxTS {
				maxTS = ts
			}
			if fired >= maxLogEventsPerPoll {
				break
			}
			s.dispatch(tr, map[string]interface{}{
				"log_group":    cfg.LogGroupName,
				"log_stream":   aws.ToString(ev.LogStreamName),
				"message":      aws.ToString(ev.Message),
				"event_id":     aws.ToString(ev.EventId),
				"timestamp":    time.UnixMilli(ts).UTC().Format(time.RFC3339),
				"triggered_at": time.Now().UTC().Format(time.RFC3339),
			})
			fired++
		}
	}

	if maxTS > sinceMs {
		wm, _ := json.Marshal(map[string]int64{"ts": maxTS})
		_ = s.db.UpsertTriggerState(tr.ID, watermarkKey, wm)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Service) dispatch(tr *launch.Trigger, data map[string]interface{}) {
	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to fire cloudwatch trigger")
	}
}

func statValue(d cwtypes.Datapoint, statistic string) float64 {
	switch statistic {
	case "Sum":
		return aws.ToFloat64(d.Sum)
	case "Minimum":
		return aws.ToFloat64(d.Minimum)
	case "Maximum":
		return aws.ToFloat64(d.Maximum)
	case "SampleCount":
		return aws.ToFloat64(d.SampleCount)
	default:
		return aws.ToFloat64(d.Average)
	}
}

func compare(value, threshold float64, op string) bool {
	switch op {
	case "GreaterThanThreshold":
		return value > threshold
	case "GreaterThanOrEqualToThreshold":
		return value >= threshold
	case "LessThanThreshold":
		return value < threshold
	case "LessThanOrEqualToThreshold":
		return value <= threshold
	default:
		return value > threshold
	}
}

func parseDimensions(raw string) []cwtypes.Dimension {
	var dims []cwtypes.Dimension
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		dims = append(dims, cwtypes.Dimension{
			Name:  aws.String(strings.TrimSpace(kv[0])),
			Value: aws.String(strings.TrimSpace(kv[1])),
		})
	}
	return dims
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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

func createCloudWatchClient(accessKey, secretKey, region string) (*cloudwatch.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}
	return cloudwatch.NewFromConfig(cfg), nil
}

func createLogsClient(accessKey, secretKey, region string) (*cloudwatchlogs.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}
	return cloudwatchlogs.NewFromConfig(cfg), nil
}
