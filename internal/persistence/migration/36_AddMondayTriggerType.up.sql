-- Add the Monday.com webhook trigger type to launch's TriggerType enum.
-- Without it, CreateTrigger's INSERT INTO trigger (type) ... = 'monday-webhook'
-- is rejected by Postgres (invalid enum value) and the trigger silently fails.
-- NUMBERING: merge AFTER 35 (asana-webhook); golang-migrate silently skips a
-- lower version applied after a higher one.
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'monday-webhook';
