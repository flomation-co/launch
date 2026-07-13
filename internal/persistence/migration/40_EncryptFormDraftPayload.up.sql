-- Encrypt draft answers at rest. Replace the plaintext payload JSONB with a
-- pgcrypto-encrypted BYTEA column (PGP_SYM_ENCRYPT keyed by the app's DB
-- encryption key, matching the credentials/blobs/secrets pattern used
-- elsewhere). Drafts are short-lived (TTL-purged) and this feature is not yet
-- deployed, so no backfill of existing rows is performed.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
ALTER TABLE form_submission_draft DROP COLUMN IF EXISTS payload;
ALTER TABLE form_submission_draft ADD COLUMN payload_enc BYTEA;
