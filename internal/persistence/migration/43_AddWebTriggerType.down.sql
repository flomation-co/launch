-- No-op: Postgres cannot drop a value from an enum type, so the 'web'
-- TriggerType label can't be removed. Leaving it in place is harmless — an
-- unused enum label. (The api-side seed row, by contrast, deletes cleanly in
-- migration 124's down.)
SELECT 1;
