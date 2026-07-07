-- Add the Asana webhook trigger type to launch's TriggerType enum.
-- Without it, CreateTrigger's INSERT INTO trigger (type) ... = 'asana-webhook'
-- is rejected by Postgres (invalid enum value) and the trigger silently fails to
-- register, so inbound webhooks 404.
--
-- NUMBERING: golang-migrate applies versions in ascending order and silently
-- skips a lower-numbered migration that lands after a higher one. Merge AFTER 34
-- (trello-webhook).
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'asana-webhook';
