BEGIN;

-- Lets a conversation reference (document, owner) TOGETHER. The database then
-- guarantees that a conversation belongs to the owner of its document, even if
-- application code has a bug.
ALTER TABLE documents
    ADD CONSTRAINT documents_id_user_key UNIQUE (id, user_id);

CREATE TABLE conversations (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL,
    document_id UUID        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT conversations_user_fk
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT conversations_document_owner_fk
        FOREIGN KEY (document_id, user_id) REFERENCES documents (id, user_id) ON DELETE CASCADE,
    -- One conversation per user per document. It is also the index for the
    -- main lookup: "this user's conversation about this document".
    CONSTRAINT conversations_user_document_key
        UNIQUE (user_id, document_id)
);

-- Deleting a document must find its conversations. The unique index above
-- starts with user_id, so it cannot serve a lookup by document_id alone.
CREATE INDEX conversations_document_idx ON conversations (document_id);

CREATE TABLE messages (
    id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    conversation_id   UUID        NOT NULL,
    role              TEXT        NOT NULL,
    content           TEXT        NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'complete',
    sources           JSONB,       -- chunk indexes that backed an answer, e.g. [3, 7]
    model             TEXT,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT messages_conversation_fk
        FOREIGN KEY (conversation_id) REFERENCES conversations (id) ON DELETE CASCADE,
    CONSTRAINT messages_role_valid
        CHECK (role IN ('user', 'assistant')),
    CONSTRAINT messages_status_valid
        CHECK (status IN ('complete', 'partial', 'error')),
    CONSTRAINT messages_content_length
        CHECK (char_length(content) <= 100000),
    -- Only an assistant message that was cut short or failed may be empty.
    CONSTRAINT messages_content_not_empty
        CHECK (content <> '' OR (role = 'assistant' AND status IN ('partial', 'error'))),
    -- Only assistant messages carry a status other than complete, sources, model and token counts.
    CONSTRAINT messages_user_has_no_metadata
        CHECK (role = 'assistant' OR
               (status = 'complete' AND sources IS NULL AND model IS NULL
                AND prompt_tokens IS NULL AND completion_tokens IS NULL)),
    CONSTRAINT messages_sources_is_array
        CHECK (sources IS NULL OR jsonb_typeof(sources) = 'array'),
    CONSTRAINT messages_tokens_nonnegative
        CHECK ((prompt_tokens IS NULL OR prompt_tokens >= 0)
           AND (completion_tokens IS NULL OR completion_tokens >= 0))
);

-- Serves "this conversation's messages in order" (and the last N, newest first),
-- and the cascade when a conversation is deleted.
CREATE INDEX messages_conversation_idx ON messages (conversation_id, id);

COMMIT;