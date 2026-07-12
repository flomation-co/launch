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
	ExpiresAt    time.Time       `db:"expires_at"`
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
			status, payment_ref, expires_at
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
// (draft, finalising, or fired) and regardless of expiry, or (nil, nil) if
// none exists. Used by the payment-completion callback, which must inspect a
// 'finalising' draft's stored payment_ref and status to verify a Stripe
// session before firing — GetFormDraft deliberately hides non-'draft' rows.
func (s *Service) GetFormDraftAny(submissionID string) (*FormDraft, error) {
	var draft FormDraft
	err := s.conn.Get(&draft, `
		SELECT submission_id, trigger_id, flow_id,
			COALESCE(PGP_SYM_DECRYPT(payload_enc, $2), '')::bytea AS payload,
			status, payment_ref, expires_at
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

// MarkFormDraftFinalising transitions a draft to the 'finalising' state and
// records an external payment reference. Used when a submission is handed off
// to a payment provider before the flow fires. Accepts a draft already in
// 'finalising' too, so a retry (the user cancelled Stripe Checkout and
// started a fresh session) can overwrite the stored payment_ref with the new
// session id. A 'fired' draft is never re-opened. Returns false when no draft
// matched (already fired, or unknown).
func (s *Service) MarkFormDraftFinalising(submissionID, paymentRef string) (bool, error) {
	result, err := s.conn.Exec(`
		UPDATE form_submission_draft
		SET status = 'finalising', payment_ref = $2, updated_at = NOW()
		WHERE submission_id = $1 AND status IN ('draft', 'finalising')
	`, submissionID, paymentRef)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
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
