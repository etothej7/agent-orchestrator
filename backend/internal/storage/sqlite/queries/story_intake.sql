-- name: GetStoryIntake :one
SELECT * FROM story_intakes WHERE id=? OR source_message_id=?;

-- name: GetStoryIntakeSource :one
SELECT m.* FROM conversation_messages m JOIN conversations c ON c.id=m.conversation_id
JOIN coordinator_human_messages h ON h.message_id=m.id AND h.purpose='human_request'
WHERE m.id=? AND c.current_session_id=?;

-- name: InsertStoryIntake :exec
INSERT INTO story_intakes(id,source_message_id,sender_session_id,request_hash,epic_id) VALUES(?,?,?,?,?);
