BEGIN;

CREATE TABLE chunks (
    id          BIGINT   GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    document_id UUID     NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_index INTEGER  NOT NULL,
    content     TEXT     NOT NULL,
    -- Postgres keeps this in sync with content. Your code never writes it.
    tsv         TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,

    CONSTRAINT chunks_index_nonnegative CHECK (chunk_index >= 0),
    CONSTRAINT chunks_document_index_key UNIQUE (document_id, chunk_index)
);

CREATE INDEX chunks_tsv_idx ON chunks USING GIN (tsv);

COMMIT;