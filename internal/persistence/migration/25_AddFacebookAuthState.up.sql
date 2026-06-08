-- R3 Phase 2: Facebook OAuth state table.
--
-- Identity-only at the start; future Facebook OAuth purposes can extend.
-- Mirrors microsoft_auth_state and slack_auth_state schemas.

CREATE TABLE IF NOT EXISTS facebook_auth_state (
    state           VARCHAR PRIMARY KEY,
    purpose         VARCHAR NOT NULL DEFAULT 'identity',
    user_id         UUID    NOT NULL,
    organisation_id UUID,
    channel_type    TEXT    NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '10 minutes')
);

CREATE INDEX IF NOT EXISTS idx_facebook_auth_state_expires ON facebook_auth_state (expires_at);
