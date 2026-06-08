-- R3 Phase 2: Slack OAuth state table.
--
-- Slack has no prior Launch-side OAuth flow (existing Slack integrations
-- are agent-config bot tokens used for webhook verification only). The
-- new "Sign in with Slack" identity flow needs one-time state binding,
-- so we add a Slack-specific table mirroring microsoft_auth_state.

CREATE TABLE IF NOT EXISTS slack_auth_state (
    state           VARCHAR PRIMARY KEY,
    purpose         VARCHAR NOT NULL DEFAULT 'identity',
    user_id         UUID    NOT NULL,
    organisation_id UUID,
    channel_type    TEXT    NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '10 minutes')
);

CREATE INDEX IF NOT EXISTS idx_slack_auth_state_expires ON slack_auth_state (expires_at);
