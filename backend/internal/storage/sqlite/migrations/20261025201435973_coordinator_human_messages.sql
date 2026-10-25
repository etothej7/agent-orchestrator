-- +goose Up
-- Explicit audience labels existing messages; no second transcript store. The
-- join follows message ownership through branch/provisioning conversation moves.
CREATE TABLE coordinator_human_messages (
 message_id TEXT PRIMARY KEY REFERENCES conversation_messages(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL CHECK(purpose IN ('human_request','scope_question','blocking_decision','completion_decision'))
);
-- Preserve pre-upgrade client-authored human history. Provider-only imports do
-- not establish human intent for new workflow adoption.
INSERT INTO coordinator_human_messages(message_id,purpose)
SELECT id,'human_request' FROM conversation_messages
WHERE role='user' AND origin='human' AND sender_session_id='' AND client_message_id<>'';
-- +goose Down
DROP TABLE coordinator_human_messages;
