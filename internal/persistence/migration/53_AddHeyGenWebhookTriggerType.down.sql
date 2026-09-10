-- No-op: Postgres cannot drop a value from an enum type. The matching api down
-- migration (145) deletes its seed row cleanly, so a full rollback leaves this
-- 'heygen-webhook' enum label orphaned but harmless (simply unused). Mirrors the
-- asymmetry documented in 52_AddApolloWebhookTriggerType.down.sql.
SELECT 1;
