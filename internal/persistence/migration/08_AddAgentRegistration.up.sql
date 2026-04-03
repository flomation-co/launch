CREATE TABLE IF NOT EXISTS agent_registration (
    agent_id                UUID PRIMARY KEY,
    orchestrator_flow_id    UUID,
    trigger_id              UUID,
    channels                JSONB NOT NULL DEFAULT '[]'::jsonb,
    environment_id          UUID,
    max_executions_per_hour INT NOT NULL DEFAULT 100,
    requires_approval       BOOLEAN NOT NULL DEFAULT FALSE,
    api_url                 TEXT NOT NULL DEFAULT '',
    registered_at           TIMESTAMP NOT NULL DEFAULT NOW(),
    disabled_at             TIMESTAMP
);
