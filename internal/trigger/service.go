package trigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"

	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/mtls"
	"flomation.app/automate/launch/internal/persistence"
)

type Service struct {
	config    *config.Config
	db        *persistence.Service
	apiClient *http.Client
}

func NewService(cfg *config.Config, db *persistence.Service) *Service {
	client, err := mtls.ClientOrDefault(cfg.TLS, 30*time.Second)
	if err != nil {
		log.WithError(err).Fatal("trigger: unable to create API client")
	}

	return &Service{
		config:    cfg,
		db:        db,
		apiClient: client,
	}
}

func (s *Service) CreateTrigger(trigger launch.Trigger) (*launch.Trigger, error) {
	t, err := s.db.CreateTrigger(trigger)
	if err != nil {
		return nil, err
	}

	return t, nil
}

func (s *Service) UpdateTrigger(trigger launch.Trigger) error {
	return s.db.UpdateTrigger(trigger)
}

func (s *Service) RemoveTrigger(trigger launch.Trigger) error {
	return s.db.RemoveTrigger(trigger)
}

func (s *Service) GetTriggerByID(id string) (*launch.Trigger, error) {
	return s.db.GetTriggerByID(id)
}

func (s *Service) GetTriggersByFlowID(flowId string) ([]*launch.Trigger, error) {
	return s.db.GetTriggersByFlowID(flowId)
}

func (s *Service) GetTriggersByType(typeName string) ([]*launch.Trigger, error) {
	return s.db.GetTriggersByType(typeName)
}

// ResolveVariables calls the API to resolve ${secrets.X} and ${env.X}
// references using the trigger's flow environment.
func (s *Service) ResolveVariables(triggerID string, variables []string) (map[string]string, error) {
	if len(variables) == 0 {
		return nil, nil
	}

	payload, err := json.Marshal(map[string]interface{}{
		"variables": variables,
	})
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%v/api/v1/trigger/%v/resolve", s.config.InternalAPIURL(), triggerID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := s.apiClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		if res.Body != nil {
			_ = res.Body.Close()
		}
	}()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("resolve variables returned status %v", res.Status)
	}

	var resolved map[string]string
	if err := json.NewDecoder(res.Body).Decode(&resolved); err != nil {
		return nil, err
	}

	return resolved, nil
}

// ResolveString resolves all ${...} variable references in a string value.
func (s *Service) ResolveString(triggerID string, value string) string {
	if !strings.Contains(value, "${") {
		return value
	}

	// Extract variable references
	var vars []string
	re := regexp.MustCompile(`\$\{([^}]+)\}`)
	matches := re.FindAllStringSubmatch(value, -1)
	for _, m := range matches {
		vars = append(vars, m[1])
	}

	if len(vars) == 0 {
		return value
	}

	resolved, err := s.ResolveVariables(triggerID, vars)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": triggerID,
		}).Warn("unable to resolve trigger variables")
		return value
	}

	result := value
	for key, val := range resolved {
		result = strings.ReplaceAll(result, "${"+key+"}", val)
	}

	return result
}

func (s *Service) Trigger(trigger *launch.Trigger, data interface{}) error {
	if trigger.DisabledAt != nil {
		return nil
	}

	log.WithFields(log.Fields{
		"id":   trigger.ID,
		"type": trigger.Type,
		"data": data,
	}).Info("invoking trigger")

	url := fmt.Sprintf("%v/api/v1/internal/flo/%v/trigger/%v/execute", s.config.InternalAPIURL(), trigger.FlowID, trigger.ID)

	b, err := json.Marshal(data)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}

	res, err := s.apiClient.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return errors.New("invalid status code: " + res.Status)
	}

	defer func() {
		if res.Body != nil {
			_ = res.Body.Close()
		}
	}()

	return nil
}
