-- Add the SendGrid webhook trigger type to launch's TriggerType enum.
-- Without it, CreateTrigger's INSERT INTO trigger (type) ... = 'sendgrid-webhook'
-- is rejected by Postgres (invalid enum value) and the trigger silently fails.
-- NUMBERING: merge AFTER 37 (intercom-webhook); golang-migrate silently skips a
-- lower version applied after a higher one.
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'sendgrid-webhook';
