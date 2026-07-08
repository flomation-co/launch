-- Add the Intercom webhook trigger type to launch's TriggerType enum.
-- Without it, CreateTrigger's INSERT INTO trigger (type) ... = 'intercom-webhook'
-- is rejected by Postgres (invalid enum value) and the trigger silently fails.
-- NUMBERING: merge AFTER 36 (monday-webhook); golang-migrate silently skips a
-- lower version applied after a higher one.
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'intercom-webhook';
