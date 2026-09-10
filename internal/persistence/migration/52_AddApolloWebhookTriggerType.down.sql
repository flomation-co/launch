-- No-op: Postgres cannot drop a value from an enum type. The matching api down
-- migration (142) deletes its seed row cleanly, so a full rollback leaves this
-- 'apollo-webhook' enum label orphaned but harmless (simply unused). Mirrors the
-- asymmetry documented in 47_AddDatabaseRowTriggerType.down.sql.
SELECT 1;
