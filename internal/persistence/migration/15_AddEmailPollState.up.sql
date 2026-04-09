-- State tracking for agent email channel polling. Similar to trigger_state
-- but without the foreign key to the trigger table, allowing agent IDs
-- and other scope keys.
CREATE TABLE IF NOT EXISTS email_poll_state (
    scope_id    TEXT NOT NULL,
    state_key   VARCHAR(1024) NOT NULL,
    state_data  JSONB NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (scope_id, state_key)
);