ALTER TABLE form_submission_draft DROP COLUMN IF EXISTS payload_enc;
ALTER TABLE form_submission_draft ADD COLUMN payload JSONB NOT NULL DEFAULT '{}'::jsonb;
