-- name: ListCoordinatorHumanMessages :many
SELECT h.message_id, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE m.conversation_id = ?;

-- name: GetCoordinatorHumanMessage :one
SELECT m.*, h.purpose FROM coordinator_human_messages h
JOIN conversation_messages m ON m.id=h.message_id WHERE h.message_id=?;

-- name: InsertCoordinatorHumanMessage :exec
INSERT INTO coordinator_human_messages(message_id,purpose) VALUES(?,?);
