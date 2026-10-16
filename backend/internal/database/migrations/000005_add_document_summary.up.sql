BEGIN;

ALTER TABLE documents
    ADD COLUMN summary            TEXT,
    ADD COLUMN summary_status     TEXT        NOT NULL DEFAULT 'none',
    ADD COLUMN summary_error      TEXT,
    ADD COLUMN summary_updated_at TIMESTAMPTZ;

ALTER TABLE documents
    ADD CONSTRAINT documents_summary_status_valid
        CHECK (summary_status IN ('none', 'processing', 'ready', 'failed')),
    ADD CONSTRAINT documents_summary_ready_has_text
        CHECK (summary_status <> 'ready' OR summary IS NOT NULL),
    ADD CONSTRAINT documents_summary_error_only_when_failed
        CHECK (summary_error IS NULL OR summary_status = 'failed');

COMMIT;