CREATE TABLE IF NOT EXISTS agent_lease (
    agent_id    UUID PRIMARY KEY,
    instance_id VARCHAR(128) NOT NULL,
    leased_at   TIMESTAMP NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_lease_expires ON agent_lease(expires_at);
