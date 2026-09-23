ALTER TABLE agent_registration
    ADD COLUMN max_executions_per_hour INT NOT NULL DEFAULT 100,
    ADD COLUMN requires_approval BOOLEAN NOT NULL DEFAULT FALSE;
