-- +goose Up
-- Explicit audience labels existing messages; no second transcript store. The
-- join follows message ownership through branch/provisioning conversation moves.
CREATE TABLE coordinator_human_messages (
 message_id TEXT PRIMARY KEY REFERENCES conversation_messages(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL CHECK(purpose IN ('human_request','scope_question','blocking_decision','completion_decision','verified_result','stopped_work','legacy_public'))
);
-- Preserve pre-upgrade human-authored history. A message typed in AO has a
-- client id, or (the opening prompt of a session, created before client ids
-- existed) no provider item either. Provider-only imports carry a provider item
-- and no client id, so they do not establish human intent for new workflow
-- adoption. The table is only consulted for the coordinator project's
-- conversation; labels elsewhere are inert.
INSERT INTO coordinator_human_messages(message_id,purpose)
SELECT id,'human_request' FROM conversation_messages
WHERE role='user' AND origin='human' AND sender_session_id=''
  AND (client_message_id<>'' OR provider_item_id='');
-- Public pre-upgrade assistant history stays visible; reasoning and provider
-- activities are separate records and are not added to this allowlist.
INSERT INTO coordinator_human_messages(message_id,purpose)
SELECT id,'legacy_public' FROM conversation_messages WHERE role='assistant';
-- +goose Down
DROP TABLE coordinator_human_messages;
