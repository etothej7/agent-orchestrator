-- name: CreateWorkspaceEpic :exec
INSERT INTO workspace_epics(id, title) VALUES (?, ?);
-- name: ListWorkspaceEpics :many
SELECT * FROM workspace_epics ORDER BY rowid;
-- name: CreateWorkspaceStory :exec
INSERT INTO workspace_stories(id, epic_id, record, revision) VALUES (?, ?, ?, 1);
-- name: ListWorkspaceStories :many
SELECT * FROM workspace_stories ORDER BY rowid;
-- name: GetWorkspaceStory :one
SELECT * FROM workspace_stories WHERE id = ?;
-- name: UpdateWorkspaceStory :execrows
UPDATE workspace_stories SET record = ?, revision = revision + 1 WHERE id = ? AND revision = ?;
-- name: CreateWorkspaceEvent :exec
INSERT INTO workspace_story_events(id, story_id, record, request) VALUES (?, ?, ?, ?);
-- name: GetWorkspaceEvent :one
SELECT * FROM workspace_story_events WHERE id = ?;
-- name: ListWorkspaceEvents :many
SELECT * FROM workspace_story_events WHERE story_id = ? ORDER BY rowid;
-- name: UpdateWorkspaceEvent :exec
UPDATE workspace_story_events SET record = ? WHERE id = ?;
-- name: GetStoryReportByClientID :one
SELECT * FROM reports WHERE session_id = ? AND client_report_id = ?;
