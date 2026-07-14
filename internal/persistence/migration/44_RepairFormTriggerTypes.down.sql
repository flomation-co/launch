-- PostgreSQL does not support removing values from enums.
-- To rollback, the enum would need to be recreated without these values.
-- Leaving the labels in place is harmless — unused enum labels. (The api-side
-- seed rows, by contrast, delete cleanly in migration 125's down.)
SELECT 1;
