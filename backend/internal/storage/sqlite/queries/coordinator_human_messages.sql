-- Explicitly labeled messages, plus assistant output that replies to the human:
-- an assistant message is a human_reply when its turn holds an earlier
-- human_request user message or a human steer activity (Ed typed into a running
-- turn). Automation-started turns have neither, so their output stays internal
-- unless it was explicitly addressed to the human. One pass computes each turn's
-- first human anchor, so the cost is linear in the conversation.
-- name: ListCoordinatorHumanMessages :many
WITH human_turns AS (
    SELECT turn_id, MIN(seq) AS first_seq FROM (
        SELECT u.turn_id AS turn_id, u.sequence AS seq FROM coordinator_human_messages hu
        JOIN conversation_messages u ON u.id = hu.message_id
        WHERE hu.purpose = 'human_request' AND u.role = 'user' AND u.turn_id IS NOT NULL
          AND u.conversation_id = sqlc.arg(conversation_id)
        UNION ALL
        SELECT s.turn_id, s.sequence FROM conversation_activities s
        WHERE s.conversation_id = sqlc.arg(conversation_id) AND s.turn_id IS NOT NULL AND s.kind='system' AND json_valid(s.detail_json) AND json_extract(s.detail_json,'$.event')='steer' AND json_extract(s.detail_json,'$.origin')='human' AND COALESCE(json_extract(s.detail_json,'$.senderSessionId'),'')=''
    ) GROUP BY turn_id
)
SELECT h.message_id, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE m.conversation_id = sqlc.arg(conversation_id)
UNION ALL
SELECT m.id AS message_id, 'human_reply' AS purpose FROM conversation_messages m
JOIN human_turns ht ON ht.turn_id = m.turn_id AND ht.first_seq < m.sequence
WHERE m.conversation_id = sqlc.arg(conversation_id) AND m.role='assistant'
  AND NOT EXISTS (SELECT 1 FROM coordinator_human_messages x WHERE x.message_id=m.id);

-- name: GetCoordinatorHumanMessage :one
SELECT m.*, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE h.message_id=?;

-- name: InsertCoordinatorHumanMessage :exec
INSERT INTO coordinator_human_messages(message_id,purpose) VALUES(?,?);

-- Coordinator notices are bounded per purpose so none becomes a status channel:
-- verified results tightly, stopped work generously (a stop must never be lost).
-- name: ListRecentCoordinatorNoticeTimes :many
SELECT m.created_at FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id
WHERE h.purpose = sqlc.arg(purpose) AND m.created_at >= sqlc.arg(since)
ORDER BY m.created_at DESC
LIMIT sqlc.arg(max_rows);

-- name: GetCoordinatorNoticeKey :one
SELECT message_id, created_at FROM coordinator_notice_keys WHERE dedupe_key = ?;

-- name: UpsertCoordinatorNoticeKey :exec
INSERT INTO coordinator_notice_keys(dedupe_key, message_id, created_at) VALUES(?, ?, ?)
ON CONFLICT(dedupe_key) DO UPDATE SET message_id = excluded.message_id, created_at = excluded.created_at;
