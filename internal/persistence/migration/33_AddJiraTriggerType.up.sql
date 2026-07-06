-- Add the Jira webhook trigger type to launch's TriggerType enum.
-- Without it, CreateTrigger's INSERT INTO trigger (type) ... = 'jira-webhook'
-- is rejected by Postgres (invalid enum value) and the trigger silently fails to
-- register in launch, so inbound webhooks 404.
--
-- Migration NUMBERING NOTE: golang-migrate applies versions in ascending order
-- and silently skips a lower-numbered migration that lands after a higher one
-- has already been applied. This must merge AFTER 32 (woocommerce-webhook).
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'jira-webhook';
