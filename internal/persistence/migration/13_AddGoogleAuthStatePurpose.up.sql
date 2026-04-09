-- Add purpose column to Google auth state so the callback knows
-- which scope set was requested (calendar, email_read, email_send).
ALTER TABLE google_auth_state
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'calendar';
