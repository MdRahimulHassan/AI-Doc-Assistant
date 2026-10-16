BEGIN;

ALTER TABLE documents
    DROP CONSTRAINT IF EXISTS documents_summary_error_only_when_failed,
    DROP CONSTRAINT IF EXISTS documents_summary_ready_has_text,
    DROP CONSTRAINT IF EXISTS documents_summary_status_valid;

ALTER TABLE documents
    DROP COLUMN IF EXISTS summary_updated_at,
    DROP COLUMN IF EXISTS summary_error,
    DROP COLUMN IF EXISTS summary_status,
    DROP COLUMN IF EXISTS summary;

COMMIT;