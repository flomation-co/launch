package persistence

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
)

type Service struct {
	config *config.Config
	conn   *sqlx.DB

	stmtCreateTrigger *sqlx.NamedStmt
	stmtUpdateTrigger *sqlx.NamedStmt
	stmtRemoveTrigger *sqlx.NamedStmt

	stmtGetTriggerByID      *sqlx.NamedStmt
	stmtGetTriggersByFlowID *sqlx.NamedStmt
	stmtGetTriggersByType   *sqlx.NamedStmt

	stmtGetTriggerState       *sqlx.NamedStmt
	stmtUpsertTriggerState    *sqlx.NamedStmt
	stmtDeleteTriggerState    *sqlx.NamedStmt
	stmtDeleteAllTriggerState *sqlx.NamedStmt
	stmtTryAcquireLease       *sqlx.NamedStmt
	stmtReleaseLease          *sqlx.NamedStmt

	// Agent statements
	stmtUpsertAgentRegistration  *sqlx.NamedStmt
	stmtGetAgentRegistration     *sqlx.NamedStmt
	stmtGetActiveAgentRegs       *sqlx.NamedStmt
	stmtDisableAgentRegistration *sqlx.NamedStmt
	stmtTryAcquireAgentLease     *sqlx.NamedStmt
	stmtReleaseAgentLease        *sqlx.NamedStmt
	stmtGetExpiredAgentLeases    *sqlx.NamedStmt
}

func NewService(config *config.Config) (*Service, error) {
	db, err := sqlx.Connect("postgres", fmt.Sprintf("postgres://%v:%v@%v:%d/%v?sslmode=%v",
		config.Database.Username,
		config.Database.Password,
		config.Database.Hostname,
		config.Database.Port,
		config.Database.Database,
		config.Database.SSLModeOverride))
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(config.Database.MaxOpenConnections)
	db.SetMaxIdleConns(config.Database.MaxIdleConnections)

	s := Service{
		config: config,
		conn:   db,
	}

	if s.stmtCreateTrigger, err = db.PrepareNamed(`
		INSERT INTO trigger (
			id,
			type,
			data,
			flow_id
		) VALUES (
			:id,
			:type,
			:data,
			:flow_id
		)
		ON CONFLICT (id) DO UPDATE SET data = :data
		RETURNING id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtCreateTrigger")
	}

	if s.stmtUpdateTrigger, err = db.PrepareNamed(`
		UPDATE trigger
		SET data = :data
		WHERE id = :id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtUpdateTrigger")
	}

	if s.stmtRemoveTrigger, err = db.PrepareNamed(`
		DELETE FROM trigger
		WHERE id = :id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtRemoveTrigger")
	}

	if s.stmtGetTriggerByID, err = db.PrepareNamed(`
		SELECT
		    id,
		    type,
		    data,
		    flow_id,
		    created_at,
		    disabled_at
		FROM
		    trigger
		WHERE 
		    id = :id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtGetTriggerByID")
	}

	if s.stmtGetTriggersByFlowID, err = db.PrepareNamed(`
		SELECT
		    id,
		    type,
		    data,
		    flow_id,
		    created_at,
		    disabled_at
		FROM
		    trigger
		WHERE 
		    flow_id = :flow_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtGetTriggersByFlowID")
	}

	if s.stmtGetTriggersByType, err = db.PrepareNamed(`
		SELECT
		    id,
		    type,
		    data,
		    flow_id,
		    created_at,
		    disabled_at
		FROM
		    trigger
		WHERE
		    type = :type
		    AND disabled_at IS NULL;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtGetTriggersByType")
	}

	if s.stmtGetTriggerState, err = db.PrepareNamed(`
		SELECT state_key, state_data
		FROM trigger_state
		WHERE trigger_id = :trigger_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtGetTriggerState")
	}

	if s.stmtUpsertTriggerState, err = db.PrepareNamed(`
		INSERT INTO trigger_state (trigger_id, state_key, state_data, updated_at)
		VALUES (:trigger_id, :state_key, :state_data, NOW())
		ON CONFLICT (trigger_id, state_key) DO UPDATE
		SET state_data = :state_data, updated_at = NOW();
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtUpsertTriggerState")
	}

	if s.stmtDeleteTriggerState, err = db.PrepareNamed(`
		DELETE FROM trigger_state
		WHERE trigger_id = :trigger_id AND state_key = :state_key;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtDeleteTriggerState")
	}

	if s.stmtDeleteAllTriggerState, err = db.PrepareNamed(`
		DELETE FROM trigger_state
		WHERE trigger_id = :trigger_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtDeleteAllTriggerState")
	}

	if s.stmtTryAcquireLease, err = db.PrepareNamed(`
		INSERT INTO trigger_lease (trigger_id, instance_id, leased_at, expires_at)
		VALUES (:trigger_id, :instance_id, NOW(), NOW() + :duration * INTERVAL '1 second')
		ON CONFLICT (trigger_id) DO UPDATE
		SET instance_id = :instance_id, leased_at = NOW(), expires_at = NOW() + :duration * INTERVAL '1 second'
		WHERE trigger_lease.expires_at < NOW() OR trigger_lease.instance_id = :instance_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtTryAcquireLease")
	}

	if s.stmtReleaseLease, err = db.PrepareNamed(`
		DELETE FROM trigger_lease
		WHERE trigger_id = :trigger_id AND instance_id = :instance_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare named statement stmtReleaseLease")
	}

	// --- Agent statements ---

	if s.stmtUpsertAgentRegistration, err = db.PrepareNamed(`
		INSERT INTO agent_registration (agent_id, orchestrator_flow_id, trigger_id, channels, environment_id,
			max_executions_per_hour, requires_approval, system_prompt, api_url)
		VALUES (:agent_id, :orchestrator_flow_id, :trigger_id, :channels, :environment_id,
			:max_executions_per_hour, :requires_approval, :system_prompt, :api_url)
		ON CONFLICT (agent_id) DO UPDATE SET
			orchestrator_flow_id = :orchestrator_flow_id, trigger_id = :trigger_id,
			channels = :channels, environment_id = :environment_id,
			max_executions_per_hour = :max_executions_per_hour, requires_approval = :requires_approval,
			system_prompt = :system_prompt, api_url = :api_url, disabled_at = NULL;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtUpsertAgentRegistration")
	}

	if s.stmtGetAgentRegistration, err = db.PrepareNamed(`
		SELECT * FROM agent_registration WHERE agent_id = :agent_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtGetAgentRegistration")
	}

	if s.stmtGetActiveAgentRegs, err = db.PrepareNamed(`
		SELECT * FROM agent_registration WHERE disabled_at IS NULL;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtGetActiveAgentRegs")
	}

	if s.stmtDisableAgentRegistration, err = db.PrepareNamed(`
		UPDATE agent_registration SET disabled_at = NOW() WHERE agent_id = :agent_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtDisableAgentRegistration")
	}

	if s.stmtTryAcquireAgentLease, err = db.PrepareNamed(`
		INSERT INTO agent_lease (agent_id, instance_id, leased_at, expires_at)
		VALUES (:agent_id, :instance_id, NOW(), NOW() + :duration * INTERVAL '1 second')
		ON CONFLICT (agent_id) DO UPDATE
		SET instance_id = :instance_id, leased_at = NOW(), expires_at = NOW() + :duration * INTERVAL '1 second'
		WHERE agent_lease.expires_at < NOW() OR agent_lease.instance_id = :instance_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtTryAcquireAgentLease")
	}

	if s.stmtReleaseAgentLease, err = db.PrepareNamed(`
		DELETE FROM agent_lease WHERE agent_id = :agent_id AND instance_id = :instance_id;
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtReleaseAgentLease")
	}

	if s.stmtGetExpiredAgentLeases, err = db.PrepareNamed(`
		SELECT r.* FROM agent_registration r
		LEFT JOIN agent_lease l ON l.agent_id = r.agent_id
		WHERE r.disabled_at IS NULL
		AND (l.agent_id IS NULL OR l.expires_at < NOW());
	`); err != nil {
		return nil, errors.Wrap(err, "unable to prepare stmtGetExpiredAgentLeases")
	}

	return &s, nil
}

func (s *Service) CreateTrigger(trigger launch.Trigger) (*launch.Trigger, error) {
	var id string
	if err := s.stmtCreateTrigger.Get(&id, trigger); err != nil {
		return nil, err
	}

	trigger.ID = id

	return &trigger, nil
}

func (s *Service) UpdateTrigger(trigger launch.Trigger) error {
	_, err := s.stmtUpdateTrigger.Exec(trigger)
	return err
}

func (s *Service) RemoveTrigger(trigger launch.Trigger) error {
	_, err := s.stmtRemoveTrigger.Exec(trigger)
	return err
}

func (s *Service) GetTriggerByID(id string) (*launch.Trigger, error) {
	var t launch.Trigger
	if err := s.stmtGetTriggerByID.Get(&t, struct {
		ID string `db:"id"`
	}{
		ID: id,
	}); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}

		return nil, err
	}

	return &t, nil
}

func (s *Service) GetTriggersByFlowID(flowId string) ([]*launch.Trigger, error) {
	var t []*launch.Trigger
	if err := s.stmtGetTriggersByFlowID.Select(&t, struct {
		FlowID string `db:"id"`
	}{
		FlowID: flowId,
	}); err != nil {
		return nil, err
	}

	return t, nil
}

func (s *Service) GetTriggersByType(triggerType string) ([]*launch.Trigger, error) {
	var t []*launch.Trigger
	if err := s.stmtGetTriggersByType.Select(&t, struct {
		Type string `db:"type"`
	}{
		Type: triggerType,
	}); err != nil {
		return nil, err
	}

	return t, nil
}

type triggerStateRow struct {
	StateKey  string          `db:"state_key"`
	StateData json.RawMessage `db:"state_data"`
}

func (s *Service) GetTriggerState(triggerID string) (map[string]json.RawMessage, error) {
	var rows []triggerStateRow
	if err := s.stmtGetTriggerState.Select(&rows, struct {
		TriggerID string `db:"trigger_id"`
	}{
		TriggerID: triggerID,
	}); err != nil {
		return nil, err
	}

	result := make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		result[r.StateKey] = r.StateData
	}

	return result, nil
}

func (s *Service) UpsertTriggerState(triggerID, stateKey string, stateData json.RawMessage) error {
	_, err := s.stmtUpsertTriggerState.Exec(struct {
		TriggerID string          `db:"trigger_id"`
		StateKey  string          `db:"state_key"`
		StateData json.RawMessage `db:"state_data"`
	}{
		TriggerID: triggerID,
		StateKey:  stateKey,
		StateData: stateData,
	})
	return err
}

func (s *Service) DeleteTriggerState(triggerID, stateKey string) error {
	_, err := s.stmtDeleteTriggerState.Exec(struct {
		TriggerID string `db:"trigger_id"`
		StateKey  string `db:"state_key"`
	}{
		TriggerID: triggerID,
		StateKey:  stateKey,
	})
	return err
}

func (s *Service) DeleteAllTriggerState(triggerID string) error {
	_, err := s.stmtDeleteAllTriggerState.Exec(struct {
		TriggerID string `db:"trigger_id"`
	}{
		TriggerID: triggerID,
	})
	return err
}

func (s *Service) TryAcquireLease(triggerID, instanceID string, duration time.Duration) (bool, error) {
	result, err := s.stmtTryAcquireLease.Exec(struct {
		TriggerID  string  `db:"trigger_id"`
		InstanceID string  `db:"instance_id"`
		Duration   float64 `db:"duration"`
	}{
		TriggerID:  triggerID,
		InstanceID: instanceID,
		Duration:   duration.Seconds(),
	})
	if err != nil {
		return false, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return rows > 0, nil
}

func (s *Service) ReleaseLease(triggerID, instanceID string) error {
	_, err := s.stmtReleaseLease.Exec(struct {
		TriggerID  string `db:"trigger_id"`
		InstanceID string `db:"instance_id"`
	}{
		TriggerID:  triggerID,
		InstanceID: instanceID,
	})
	return err
}
