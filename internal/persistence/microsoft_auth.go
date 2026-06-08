package persistence

import (
	"database/sql"
	"fmt"
	"time"

	"flomation.app/automate/launch"
)

// CreateMicrosoftAuthStateIdentity stores a one-time state token for the
// Microsoft user-identity OAuth flow (R3 Phase 2). Mirrors the equivalent
// Google helper. organisationID may be empty for personal mode.
func (s *Service) CreateMicrosoftAuthStateIdentity(state, userID, organisationID, channelType string) error {
	if userID == "" {
		return fmt.Errorf("user_id is required")
	}
	if channelType == "" {
		return fmt.Errorf("channel_type is required")
	}
	var orgIDParam interface{}
	if organisationID != "" {
		orgIDParam = organisationID
	}
	_, err := s.conn.Exec(
		`INSERT INTO microsoft_auth_state (state, purpose, user_id, organisation_id, channel_type)
		 VALUES ($1, 'identity', $2, $3, $4)`,
		state, userID, orgIDParam, channelType,
	)
	return err
}

// ConsumeMicrosoftAuthState validates and deletes a state token, returning
// the bound (user_id, organisation_id, channel_type) tuple. One-time use.
func (s *Service) ConsumeMicrosoftAuthState(state string) (*launch.MicrosoftAuthState, error) {
	// Clean up expired states first
	_, _ = s.conn.Exec(`DELETE FROM microsoft_auth_state WHERE expires_at < $1`, time.Now())

	var row struct {
		State          string  `db:"state"`
		Purpose        string  `db:"purpose"`
		UserID         string  `db:"user_id"`
		OrganisationID *string `db:"organisation_id"`
		ChannelType    string  `db:"channel_type"`
	}
	err := s.conn.Get(&row,
		`SELECT state, purpose, user_id, organisation_id, channel_type
		 FROM microsoft_auth_state WHERE state = $1 AND expires_at > $2`,
		state, time.Now())
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("state token not found or expired")
		}
		return nil, err
	}
	_, _ = s.conn.Exec(`DELETE FROM microsoft_auth_state WHERE state = $1`, state)

	out := &launch.MicrosoftAuthState{
		State:       row.State,
		Purpose:     row.Purpose,
		UserID:      row.UserID,
		ChannelType: row.ChannelType,
	}
	if row.OrganisationID != nil {
		out.OrganisationID = *row.OrganisationID
	}
	return out, nil
}
