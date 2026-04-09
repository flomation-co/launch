-- Add trigger_id to auth state for trigger-scoped OAuth flows.
-- When non-null, the callback stores the token against the trigger
-- instead of an agent_user.
ALTER TABLE google_auth_state
    ADD COLUMN IF NOT EXISTS trigger_id TEXT;

-- Make agent_user_id and agent_id nullable for trigger-scoped flows
ALTER TABLE google_auth_state
    ALTER COLUMN agent_user_id DROP NOT NULL,
    ALTER COLUMN agent_id DROP NOT NULL;