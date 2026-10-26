-- Explicitly labeled messages, plus assistant output that replies to the human:
-- an assistant message is a human_reply when its turn holds an earlier
-- human_request user message or a human steer activity (Ed typed into a running
-- turn). Automation-started turns have neither, so their output stays internal
-- unless it was explicitly addressed to the human.
-- name: ListCoordinatorHumanMessages :many
SELECT h.message_id, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE m.conversation_id = sqlc.arg(conversation_id)
UNION ALL
SELECT m.id AS message_id, 'human_reply' AS purpose FROM conversation_messages m
WHERE m.conversation_id = sqlc.arg(conversation_id) AND m.role='assistant' AND m.turn_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM coordinator_human_messages x WHERE x.message_id=m.id)
  AND (EXISTS (
      SELECT 1 FROM conversation_messages u JOIN coordinator_human_messages hu ON hu.message_id=u.id AND hu.purpose='human_request'
      WHERE u.conversation_id=m.conversation_id AND u.turn_id=m.turn_id AND u.role='user' AND u.sequence < m.sequence)
      OR EXISTS (
      SELECT 1 FROM conversation_activities s
      WHERE s.conversation_id=m.conversation_id AND s.turn_id=m.turn_id AND s.sequence < m.sequence AND (s.kind='system' AND json_valid(s.detail_json) AND json_extract(s.detail_json,'$.event')='steer' AND json_extract(s.detail_json,'$.origin')='human' AND json_extract(s.detail_json,'$.senderSessionId') IS NULL)));

-- name: GetCoordinatorHumanMessage :one
SELECT m.*, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE h.message_id=?;

-- name: InsertCoordinatorHumanMessage :exec
INSERT INTO coordinator_human_messages(message_id,purpose) VALUES(?,?);

-- Verified results are the only non-blocking coordinator notice that is
-- bounded, so they cannot become a status channel. stopped_work is never
-- limited: losing a stop would leave Ed unaware that work halted.
-- name: ListRecentCoordinatorResultTimes :many
SELECT m.created_at FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id
WHERE h.purpose = 'verified_result' AND m.created_at >= sqlc.arg(since)
ORDER BY m.created_at DESC
LIMIT sqlc.arg(max_rows);
