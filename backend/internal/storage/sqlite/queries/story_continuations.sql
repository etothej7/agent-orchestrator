-- name: GetStoryContinuation :one
SELECT * FROM story_continuations WHERE story_id = ?;
-- name: InsertStoryContinuation :execrows
INSERT INTO story_continuations(story_id, record) VALUES (?, ?) ON CONFLICT(story_id) DO NOTHING;
-- name: UpdateStoryContinuation :execrows
UPDATE story_continuations SET record = ?, revision = revision + 1 WHERE story_id = ? AND revision = ?;
