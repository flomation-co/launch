-- No-op: Postgres cannot drop a value from an enum type, so the 'awx-webhook'
-- TriggerType label can't be removed. Leaving it in place is harmless — an
-- unused enum label. (The api-side seed row, by contrast, deletes cleanly in
-- api migration 126's down.)
SELECT 1;
