-- Temporary state tokens for the Google OAuth2 flow. Each row links
-- a one-time state parameter to the agent_user_id that initiated the
-- auth flow. Rows expire after 10 minutes and are deleted on use.

CREATE TABLE IF NOT EXISTS google_auth_state (
    state           TEXT PRIMARY KEY,
    agent_user_id   UUID NOT NULL,
    agent_id        UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '10 minutes'
);

CREATE INDEX IF NOT EXISTS idx_google_auth_state_expires
    ON google_auth_state(expires_at);