-- Explicitly labeled messages, plus assistant output that replies to the human:
-- an assistant message is a human_reply when its turn holds an earlier
-- human_request user message. Automation-started turns have none, so their
-- output stays internal unless it was explicitly addressed to the human.
-- name: ListCoordinatorHumanMessages :many
SELECT h.message_id, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE m.conversation_id = sqlc.arg(conversation_id)
UNION ALL
SELECT m.id AS message_id, 'human_reply' AS purpose FROM conversation_messages m
WHERE m.conversation_id = sqlc.arg(conversation_id) AND m.role='assistant' AND m.turn_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM coordinator_human_messages x WHERE x.message_id=m.id)
  AND EXISTS (
      SELECT 1 FROM conversation_messages u JOIN coordinator_human_messages hu ON hu.message_id=u.id AND hu.purpose='human_request'
      WHERE u.turn_id=m.turn_id AND u.role='user' AND u.sequence < m.sequence);

-- name: GetCoordinatorHumanMessage :one
SELECT m.*, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE h.message_id=?;

-- name: InsertCoordinatorHumanMessage :exec
INSERT INTO coordinator_human_messages(message_id,purpose) VALUES(?,?);

-- Non-blocking notices (verified results, stopped work) are the only way a
-- coordinator can address the human without a question, so they are bounded.
-- name: CountCoordinatorNoticesSince :one
SELECT COUNT(*) FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id
WHERE h.purpose IN ('verified_result','stopped_work') AND m.created_at >= sqlc.arg(since);
