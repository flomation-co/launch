-- Per-field mid-state for native form drafts. Some fields resolve out-of-band
-- (the user acts on a field, is redirected to an external service, and returns
-- with THAT field updated) while the rest of the form stays in progress. This
-- generic state map holds each such field's opaque state, keyed by field name:
--   { "<field>": { "type": "payment", "status": "pending|complete", ... } }
-- Payment is the first consumer (Stripe: pending → complete). The map carries
-- no PII (field names, opaque provider ids, amounts the user already sees), so
-- unlike the answers payload it is stored in the clear.
ALTER TABLE form_submission_draft
    ADD COLUMN field_states JSONB NOT NULL DEFAULT '{}'::jsonb;
