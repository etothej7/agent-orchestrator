-- +goose Up
-- +goose StatementBegin
-- The 20261025201435973 table is already applied on installs that carry the
-- first coordinator-audience build, so it is widened here rather than edited in
-- place. SQLite cannot widen a CHECK, so rebuild it and keep every row.
CREATE TABLE coordinator_human_messages_next (
 message_id TEXT PRIMARY KEY REFERENCES conversation_messages(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL CHECK(purpose IN ('human_request','scope_question','blocking_decision','completion_decision','verified_result','stopped_work','legacy_public'))
);
INSERT INTO coordinator_human_messages_next(message_id, purpose)
SELECT message_id, purpose FROM coordinator_human_messages;
DROP TABLE coordinator_human_messages;
ALTER TABLE coordinator_human_messages_next RENAME TO coordinator_human_messages;

-- A session's opening prompt predates client ids: human origin, no sender, no
-- client id and no provider item. Provider imports carry a provider item and
-- stay unlabeled. Rows already labeled keep their label.
INSERT OR IGNORE INTO coordinator_human_messages(message_id, purpose)
SELECT id, 'human_request' FROM conversation_messages
WHERE role='user' AND origin='human' AND sender_session_id='' AND client_message_id='' AND provider_item_id='';

-- Reply visibility looks up the human message that started an assistant turn.
-- Leading with conversation_id keeps every conversation-scoped query seeking by
-- conversation, as the lineage query plans require.
CREATE INDEX IF NOT EXISTS idx_conversation_messages_turn
    ON conversation_messages(conversation_id, turn_id) WHERE turn_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_conversation_messages_turn;
DELETE FROM coordinator_human_messages WHERE purpose IN ('verified_result','stopped_work');
CREATE TABLE coordinator_human_messages_previous (
 message_id TEXT PRIMARY KEY REFERENCES conversation_messages(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL CHECK(purpose IN ('human_request','scope_question','blocking_decision','completion_decision','legacy_public'))
);
INSERT INTO coordinator_human_messages_previous(message_id, purpose)
SELECT message_id, purpose FROM coordinator_human_messages;
DROP TABLE coordinator_human_messages;
ALTER TABLE coordinator_human_messages_previous RENAME TO coordinator_human_messages;
-- +goose StatementEnd
