ALTER TABLE google_auth_state
    DROP COLUMN IF EXISTS channel_type,
    DROP COLUMN IF EXISTS organisation_id,
    DROP COLUMN IF EXISTS user_id;
