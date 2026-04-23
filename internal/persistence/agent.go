package persistence

import (
	"database/sql"
	"encoding/json"
	"time"

	"flomation.app/automate/launch"
)

// UpsertAgentRegistration registers or updates an agent for runtime management.
func (s *Service) UpsertAgentRegistration(reg launch.AgentRegistration) error {
	channels := reg.Channels
	if channels == nil {
		channels = json.RawMessage("[]")
	}

	_, err := s.stmtUpsertAgentRegistration.Exec(struct {
		AgentID              string          `db:"agent_id"`
		OrchestratorFlowID   *string         `db:"orchestrator_flow_id"`
		TriggerID            *string         `db:"trigger_id"`
		Channels             json.RawMessage `db:"channels"`
		EnvironmentID        *string         `db:"environment_id"`
		MaxExecutionsPerHour int             `db:"max_executions_per_hour"`
		RequiresApproval     bool            `db:"requires_approval"`
		SystemPrompt         *string         `db:"system_prompt"`
		APIURL               string          `db:"api_url"`
	}{
		AgentID:              reg.AgentID,
		OrchestratorFlowID:   reg.OrchestratorFlowID,
		TriggerID:            reg.TriggerID,
		Channels:             channels,
		EnvironmentID:        reg.EnvironmentID,
		MaxExecutionsPerHour: reg.MaxExecutionsPerHour,
		RequiresApproval:     reg.RequiresApproval,
		SystemPrompt:         reg.SystemPrompt,
		APIURL:               reg.APIURL,
	})
	return err
}

// GetAgentRegistration returns a specific agent registration.
func (s *Service) GetAgentRegistration(agentID string) (*launch.AgentRegistration, error) {
	var reg launch.AgentRegistration
	if err := s.stmtGetAgentRegistration.Get(&reg, struct {
		AgentID string `db:"agent_id"`
	}{AgentID: agentID}); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &reg, nil
}

// GetActiveAgentRegistrations returns all non-disabled agent registrations.
func (s *Service) GetActiveAgentRegistrations() ([]*launch.AgentRegistration, error) {
	var results []*launch.AgentRegistration
	// stmtGetActiveAgentRegs requires a named param but has no WHERE bind — use empty struct
	if err := s.stmtGetActiveAgentRegs.Select(&results, struct{}{}); err != nil {
		return nil, err
	}
	return results, nil
}

// DisableAgentRegistration marks an agent as disabled (stopped).
func (s *Service) DisableAgentRegistration(agentID string) error {
	_, err := s.stmtDisableAgentRegistration.Exec(struct {
		AgentID string `db:"agent_id"`
	}{AgentID: agentID})
	return err
}

// TryAcquireAgentLease attempts to acquire a lease for an agent.
// Returns true if the lease was acquired or renewed.
func (s *Service) TryAcquireAgentLease(agentID, instanceID string, duration time.Duration) (bool, error) {
	result, err := s.stmtTryAcquireAgentLease.Exec(struct {
		AgentID    string  `db:"agent_id"`
		InstanceID string  `db:"instance_id"`
		Duration   float64 `db:"duration"`
	}{
		AgentID:    agentID,
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

// ReleaseAgentLease releases the lease held by this instance.
func (s *Service) ReleaseAgentLease(agentID, instanceID string) error {
	_, err := s.stmtReleaseAgentLease.Exec(struct {
		AgentID    string `db:"agent_id"`
		InstanceID string `db:"instance_id"`
	}{AgentID: agentID, InstanceID: instanceID})
	return err
}

// ExpireAllAgentLeases removes all agent leases. Used on startup so this
// instance can immediately claim agents without waiting for the previous
// instance's leases to time out.
func (s *Service) ExpireAllAgentLeases() error {
	_, err := s.conn.Exec("DELETE FROM agent_lease")
	return err
}

// GetExpiredAgentLeases returns agent registrations that are active but have no lease or an expired lease.
// These are candidates for another instance to claim.
func (s *Service) GetExpiredAgentLeases() ([]*launch.AgentRegistration, error) {
	var results []*launch.AgentRegistration
	if err := s.stmtGetExpiredAgentLeases.Select(&results, struct{}{}); err != nil {
		return nil, err
	}
	return results, nil
}
