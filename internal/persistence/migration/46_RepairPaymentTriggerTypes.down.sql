-- Postgres cannot drop a value from an enum type, so there is nothing to undo
-- here (the same as launch migrations 44/43/41). The added values are harmless
-- if unused.
SELECT 1;
