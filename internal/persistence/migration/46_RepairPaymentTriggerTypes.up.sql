-- Repair migration: the Stripe, QuickBooks and Xero webhook integrations were
-- shipped with their Go trigger-type constants (types.go) and full inbound
-- handlers (internal/stripe, internal/quickbooks, internal/xero, wired in
-- service.go handleStripeWebhook/handleQuickBooksWebhook/handleXeroWebhook), but
-- their trigger-type values were never added to launch's TriggerType enum, and
-- the api never seeded its matching trigger_type rows either.
--
-- Consequence on every launch DB (verified absent from pg_enum on the dev stack
-- and from the whole migration source on main): CreateTrigger's
--   INSERT INTO trigger (type) VALUES
--     ('stripe-webhook' | 'quickbooks-webhook' | 'xero-webhook')
-- is rejected by Postgres (invalid enum value), so a flow using one of these
-- triggers saves 201 but no trigger row is created, no webhook is registered
-- with the provider, and inbound deliveries 404 -- silently.
--
-- Same defect class as migration 44 (RepairFormTriggerTypes, which fixed
-- typeform/jotform/surveymonkey) and 31 (RepairCalcomAcuityTriggerType). A full
-- audit of types.go vs the seeds turned up exactly these three remaining gaps.
-- Pairs with api migration 128, which seeds the api-side trigger_type rows. Both
-- are required.
--
-- All three are added idempotently, so a hand-run hotfix and this migration
-- cannot conflict.
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'stripe-webhook';
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'quickbooks-webhook';
ALTER TYPE TriggerType ADD VALUE IF NOT EXISTS 'xero-webhook';
