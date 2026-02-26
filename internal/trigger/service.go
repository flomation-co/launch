package trigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"

	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
)

type Service struct {
	config *config.Config
	db     *persistence.Service
}

func NewService(config *config.Config, db *persistence.Service) *Service {
	s := Service{
		config: config,
		db:     db,
	}

	return &s
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

func (s *Service) GetTriggerByID(id string) (*launch.Trigger, error) {
	return s.db.GetTriggerByID(id)
}

func (s *Service) GetTriggersByFlowID(flowId string) ([]*launch.Trigger, error) {
	return s.db.GetTriggersByFlowID(flowId)
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

	url := fmt.Sprintf("%v/api/v1/flo/%v/trigger/%v/execute", s.config.Automate.URL, trigger.FlowID, trigger.ID)

	client := http.Client{
		Timeout: time.Second * 30,
	}

	b, err := json.Marshal(data)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}

	res, err := client.Do(req)
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
