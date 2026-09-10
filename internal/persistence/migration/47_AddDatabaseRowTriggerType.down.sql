-- No-op: Postgres cannot drop a value from an enum type. The matching api down
-- migration (131) deletes its seed row cleanly, so a full rollback leaves this
-- 'database-row' enum label orphaned but harmless (simply unused). Mirrors the
-- asymmetry documented in 45_AddAWXTriggerType.down.sql.
SELECT 1;
