-- No-op: Postgres cannot drop a value from an enum type. The matching api down
-- migration (132) deletes its seed row cleanly, so a full rollback leaves this
-- 'rds-event' enum label orphaned but harmless (simply unused). Mirrors the
-- asymmetry documented in 47_AddDatabaseRowTriggerType.down.sql.
SELECT 1;
