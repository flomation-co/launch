-- No-op: Postgres cannot drop a value from an enum type. The matching api down
-- migration deletes its seed row; this label is left orphaned but harmless.
SELECT 1;
