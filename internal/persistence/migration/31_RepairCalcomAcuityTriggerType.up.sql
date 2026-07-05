-- Repair migration: the Cal.com and Acuity webhook integrations were shipped
-- with their Go trigger-type constants (types.go) and HTTP handlers, but their
-- trigger-type values were never added to launch's TriggerType enum. On a
-- freshly-migrated launch DB, CreateTrigger's
--   INSERT INTO trigger (type) VALUES ('calcom-webhook' | 'acuity-webhook')
-- is rejected by Postgres (invalid enum value), so those webhook triggers
-- silently fail to register and inbound deliveries 404. The api side already
-- seeded its trigger_type rows (migrations 107/109); only this enum was missed.
--
-- This mirrors api's migration 106 (AddMissingWebhookTriggerTypes). Both values
-- are added idempotently.
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'calcom-webhook';
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'acuity-webhook';
