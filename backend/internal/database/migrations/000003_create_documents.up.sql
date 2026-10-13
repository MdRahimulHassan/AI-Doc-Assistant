BEGIN;

CREATE TABLE documents (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename      TEXT        NOT NULL,                      -- original name, display only
    mime_type     TEXT        NOT NULL,
    size_bytes    BIGINT      NOT NULL,
    storage_path  TEXT        NOT NULL,                      -- relative to UPLOAD_DIR
    status        TEXT        NOT NULL DEFAULT 'processing',
    error_message TEXT,                                      -- only set when status = 'failed'
    chunk_count   INTEGER     NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT documents_status_valid
        CHECK (status IN ('processing', 'ready', 'failed')),
    CONSTRAINT documents_size_nonnegative
        CHECK (size_bytes >= 0),
    CONSTRAINT documents_chunk_count_nonnegative
        CHECK (chunk_count >= 0)
);

-- Every list query is "my documents, newest first".
CREATE INDEX documents_user_created_idx ON documents (user_id, created_at DESC);

COMMIT;