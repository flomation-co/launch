CREATE TABLE form_submission_draft (
    submission_id UUID PRIMARY KEY,
    trigger_id    UUID NOT NULL,
    flow_id       UUID,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    status        TEXT NOT NULL DEFAULT 'draft',
    payment_ref   TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_form_submission_draft_trigger ON form_submission_draft(trigger_id);
CREATE INDEX idx_form_submission_draft_expires ON form_submission_draft(expires_at);
