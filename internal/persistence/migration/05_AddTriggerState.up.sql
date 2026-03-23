CREATE TABLE IF NOT EXISTS trigger_state (
    trigger_id UUID NOT NULL REFERENCES trigger(id) ON DELETE CASCADE,
    state_key  VARCHAR(1024) NOT NULL,
    state_data JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (trigger_id, state_key)
);

CREATE TABLE IF NOT EXISTS trigger_lease (
    trigger_id  UUID PRIMARY KEY REFERENCES trigger(id) ON DELETE CASCADE,
    instance_id VARCHAR(128) NOT NULL,
    leased_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_trigger_lease_expires ON trigger_lease(expires_at);
