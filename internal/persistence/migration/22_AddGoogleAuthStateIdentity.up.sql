-- R3 Phase 2: User-identity OAuth flow.
--
-- The existing google_auth_state table records (agent_id, agent_user_id,
-- purpose, trigger_id) so the callback can route the resulting token to
-- either an agent_user_google_account or a trigger_google_account row.
--
-- Adds three nullable columns that the new identity-OAuth handler
-- populates instead — user_id + organisation_id (nullable for personal
-- mode) + channel_type. When the callback sees user_id set in the
-- consumed state, it routes the resolved external_id to the API's
-- internal user_identity endpoint rather than the existing
-- agent_user or trigger endpoints. All three flows now coexist in the
-- same callback handler.

ALTER TABLE google_auth_state
    ADD COLUMN user_id         UUID,
    ADD COLUMN organisation_id UUID,
    ADD COLUMN channel_type    TEXT;
