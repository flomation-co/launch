-- R3 Phase 2: Microsoft OAuth state table.
--
-- Microsoft OAuth doesn't currently have a Launch-side initiate/callback
-- flow (existing Microsoft integrations go through the API's generic
-- environment_credential route). The new identity-purpose flow needs
-- one-time state storage to bind a consent callback back to the user
-- who initiated it, so we add a Microsoft-specific state table here.
--
-- Schema mirrors google_auth_state but is identity-only at the start —
-- future agent/trigger Microsoft OAuth flows can add agent_id /
-- trigger_id columns when they need them.

CREATE TABLE IF NOT EXISTS microsoft_auth_state (
    state           VARCHAR PRIMARY KEY,
    purpose         VARCHAR NOT NULL DEFAULT 'identity',
    user_id         UUID    NOT NULL,
    organisation_id UUID,
    channel_type    TEXT    NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '10 minutes')
);

CREATE INDEX IF NOT EXISTS idx_microsoft_auth_state_expires ON microsoft_auth_state (expires_at);
