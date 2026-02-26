package persistence

import (
	"database/sql"
	"fmt"

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
}

func NewService(config *config.Config) (*Service, error) {
	db, err := sqlx.Connect("postgres", fmt.Sprintf("postgres://%v:%v@%v:%d/%v",
		config.Database.Username,
		config.Database.Password,
		config.Database.Hostname,
		config.Database.Port,
		config.Database.Database))
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
			type,
			data,
			flow_id                
		) VALUES (
			:type,
			:data,
			:flow_id
		) RETURNING id;
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
