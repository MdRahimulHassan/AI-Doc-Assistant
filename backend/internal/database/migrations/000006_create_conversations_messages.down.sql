BEGIN;

DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversations;

ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_id_user_key;

COMMIT;