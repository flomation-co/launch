package persistence

import (
	"database/sql"
	"fmt"
	"time"

	"flomation.app/automate/launch"
)

// CreateGoogleAuthState stores a one-time state token for an OAuth flow.
// For agent-user scoped flows, pass agentID + agentUserID.
// For trigger-scoped flows, pass triggerID (agentID/agentUserID can be empty).
func (s *Service) CreateGoogleAuthState(state, agentID, agentUserID, purpose, triggerID string) error {
	if purpose == "" {
		purpose = "calendar"
	}
	// Pass nil for empty UUID fields to avoid type mismatch (uuid vs text)
	var agentIDParam, agentUserIDParam, triggerIDParam interface{}
	if agentID != "" {
		agentIDParam = agentID
	}
	if agentUserID != "" {
		agentUserIDParam = agentUserID
	}
	if triggerID != "" {
		triggerIDParam = triggerID
	}
	_, err := s.conn.Exec(
		`INSERT INTO google_auth_state (state, agent_id, agent_user_id, purpose, trigger_id) VALUES ($1, $2, $3, $4, $5)`,
		state, agentIDParam, agentUserIDParam, purpose, triggerIDParam,
	)
	return err
}

// ConsumeGoogleAuthState validates and deletes a state token, returning
// the associated context including purpose and optional trigger_id.
func (s *Service) ConsumeGoogleAuthState(state string) (*launch.GoogleAuthState, error) {
	// Clean up expired states first
	_, _ = s.conn.Exec(`DELETE FROM google_auth_state WHERE expires_at < $1`, time.Now())

	var result struct {
		State       string  `db:"state"`
		AgentID     *string `db:"agent_id"`
		AgentUserID *string `db:"agent_user_id"`
		Purpose     string  `db:"purpose"`
		TriggerID   *string `db:"trigger_id"`
	}
	err := s.conn.Get(&result, `SELECT state, agent_id, agent_user_id, purpose, trigger_id FROM google_auth_state WHERE state = $1 AND expires_at > $2`, state, time.Now())
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("state token not found or expired")
		}
		return nil, err
	}

	// Delete the token (one-time use)
	_, _ = s.conn.Exec(`DELETE FROM google_auth_state WHERE state = $1`, state)

	authState := &launch.GoogleAuthState{
		State:   result.State,
		Purpose: result.Purpose,
	}
	if result.AgentID != nil {
		authState.AgentID = *result.AgentID
	}
	if result.AgentUserID != nil {
		authState.AgentUserID = *result.AgentUserID
	}
	if result.TriggerID != nil {
		authState.TriggerID = *result.TriggerID
	}

	return authState, nil
}
