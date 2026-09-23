-- The API no longer sends these: nothing in Launch ever read them, so
-- they were stored and never consulted. The agent runtime holds the
-- registration throughout and consults neither a rate limit nor an
-- approval gate, because neither exists.
ALTER TABLE agent_registration
    DROP COLUMN max_executions_per_hour,
    DROP COLUMN requires_approval;
