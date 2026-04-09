ALTER TABLE agent_registration
    ADD COLUMN conversation_history_limit INT NOT NULL DEFAULT 20;
