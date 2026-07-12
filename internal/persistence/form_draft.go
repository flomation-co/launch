package persistence

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"
)

// FormDraft is a server-side, autosaved copy of an in-progress native web
// form submission. It underpins draft resume (a user can close the tab and
// return) and gives submit a fire-once claim so a double-submit (or a
// payment callback) can never trigger the flow twice.
type FormDraft struct {
	SubmissionID string          `db:"submission_id"`
	TriggerID    string          `db:"trigger_id"`
	FlowID       sql.NullString  `db:"flow_id"`
	Payload      json.RawMessage `db:"payload"`
	Status       string          `db:"status"`
	PaymentRef   sql.NullString  `db:"payment_ref"`
	// FieldStates is the generic per-field mid-state map (see migration 41):
	// { "<field>": { "type":"payment", "status":"pending|complete", ... } }.
	// The draft stays 'draft' while these resolve out-of-band; each stateful
	// field type reads/writes its own opaque object via SetFieldState.
	FieldStates json.RawMessage `db:"field_states"`
	ExpiresAt   time.Time       `db:"expires_at"`
}

// CreateFormDraft inserts a fresh draft with the given TTL. It is a no-op if
// the submission id already exists (ON CONFLICT DO NOTHING), so a page reload
// that re-mints against the same id is harmless. An empty flowID is stored as
// NULL.
func (s *Service) CreateFormDraft(submissionID, triggerID, flowID string, ttl time.Duration) error {
	var flow sql.NullString
	if flowID != "" {
		flow = sql.NullString{String: flowID, Valid: true}
	}
	_, err := s.conn.Exec(`
		INSERT INTO form_submission_draft (submission_id, trigger_id, flow_id, expires_at)
		VALUES ($1, $2, $3, NOW() + ($4 * INTERVAL '1 second'))
		ON CONFLICT (submission_id) DO NOTHING
	`, submissionID, triggerID, flow, ttl.Seconds())
	return err
}

// GetFormDraft returns a live (unexpired, still-draft) draft, or (nil, nil) if
// none matches.
func (s *Service) GetFormDraft(submissionID string) (*FormDraft, error) {
	var draft FormDraft
	err := s.conn.Get(&draft, `
		SELECT submission_id, trigger_id, flow_id,
			COALESCE(PGP_SYM_DECRYPT(payload_enc, $2), '')::bytea AS payload,
			status, payment_ref, field_states, expires_at
		FROM form_submission_draft
		WHERE submission_id = $1 AND status = 'draft' AND expires_at > NOW()
	`, submissionID, s.config.Database.EncryptionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &draft, nil
}

// GetFormDraftAny returns a draft by submission id regardless of its status
// (draft or fired) and regardless of expiry, or (nil, nil) if none exists.
// Used by the payment intent/completion handlers, which must load a draft to
// inspect its field_states even in edge states — GetFormDraft deliberately
// hides non-'draft' rows.
func (s *Service) GetFormDraftAny(submissionID string) (*FormDraft, error) {
	var draft FormDraft
	err := s.conn.Get(&draft, `
		SELECT submission_id, trigger_id, flow_id,
			COALESCE(PGP_SYM_DECRYPT(payload_enc, $2), '')::bytea AS payload,
			status, payment_ref, field_states, expires_at
		FROM form_submission_draft
		WHERE submission_id = $1
	`, submissionID, s.config.Database.EncryptionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &draft, nil
}

// SaveFormDraftPayload overwrites the payload of a live draft. Returns false if
// no live draft matched (expired, fired, or unknown) so the caller can 404.
func (s *Service) SaveFormDraftPayload(submissionID string, payload json.RawMessage) (bool, error) {
	// Encrypt the answers at rest (pgcrypto, keyed by the app's DB encryption
	// key — same pattern as credentials/blobs/secrets). string(payload) binds
	// as text so PGP_SYM_ENCRYPT(text, text) applies.
	result, err := s.conn.Exec(`
		UPDATE form_submission_draft
		SET payload_enc = PGP_SYM_ENCRYPT($2, $3), updated_at = NOW()
		WHERE submission_id = $1 AND status = 'draft' AND expires_at > NOW()
	`, submissionID, string(payload), s.config.Database.EncryptionKey)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// SetFieldState writes one field's mid-state object into the draft's generic
// field_states map (see migration 41), keyed by field name. This is the single
// storage primitive shared by every stateful field type — payment writes a
// {"type":"payment","status":"pending|complete",...} object, a future e-sign
// field would write its own {"type":"esign","status":"signed",...}, etc. The
// object clobbers any previous state for that field (a field's state machine is
// linear: pending → complete), leaving other fields untouched. Only a live
// 'draft' row is updated — a fired/expired draft is immutable. Returns false
// when no live draft matched. state must be a valid JSON object.
func (s *Service) SetFieldState(submissionID, field string, state json.RawMessage) (bool, error) {
	result, err := s.conn.Exec(`
		UPDATE form_submission_draft
		SET field_states = jsonb_set(field_states, ARRAY[$2]::text[], $3::jsonb, true),
			updated_at = NOW()
		WHERE submission_id = $1 AND status = 'draft' AND expires_at > NOW()
	`, submissionID, field, string(state))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// GetFieldStates returns the draft's per-field state map (field name → opaque
// state object), regardless of the draft's status so the submit gate can read
// it right up to the fire-once claim. An absent draft yields an empty map, not
// an error. Callers dispatch on each state's "type" to verify/interpret it.
func (s *Service) GetFieldStates(submissionID string) (map[string]json.RawMessage, error) {
	var raw json.RawMessage
	err := s.conn.Get(&raw, `
		SELECT field_states FROM form_submission_draft WHERE submission_id = $1
	`, submissionID)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	states := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &states); uerr != nil {
			return nil, uerr
		}
	}
	return states, nil
}

// FireFormDraft atomically claims a draft for firing, transitioning it to
// 'fired' only if its current status is one of fromStatuses. This is the
// fire-once guard: the first caller wins (returns true + the payload), any
// subsequent caller sees no matching row and returns false. sql.ErrNoRows
// (already fired, expired-away, or unknown) resolves to (false, nil, nil).
func (s *Service) FireFormDraft(submissionID string, fromStatuses []string) (bool, json.RawMessage, error) {
	var payload json.RawMessage
	err := s.conn.QueryRow(`
		UPDATE form_submission_draft
		SET status = 'fired', updated_at = NOW()
		WHERE submission_id = $1 AND status = ANY($2)
		RETURNING COALESCE(PGP_SYM_DECRYPT(payload_enc, $3), '')::bytea
	`, submissionID, pq.Array(fromStatuses), s.config.Database.EncryptionKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, payload, nil
}

// PurgeExpiredFormDrafts deletes drafts whose TTL has lapsed and returns the
// number of rows removed. Run periodically by a background loop.
func (s *Service) PurgeExpiredFormDrafts() (int64, error) {
	result, err := s.conn.Exec(`DELETE FROM form_submission_draft WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
