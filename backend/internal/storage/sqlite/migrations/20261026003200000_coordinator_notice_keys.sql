-- +goose Up
-- +goose StatementBegin
-- A coordinator that repeats the same question, result or stop under a fresh id
-- must not reach the human twice. The key is the purpose, story and normalized
-- text; the earlier message is returned while it is inside the dedupe window.
CREATE TABLE coordinator_notice_keys (
 dedupe_key TEXT PRIMARY KEY,
 message_id TEXT NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
 created_at TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE coordinator_notice_keys;
-- +goose StatementEnd
