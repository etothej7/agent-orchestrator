-- +goose Up
CREATE TABLE story_continuations (
 story_id TEXT PRIMARY KEY REFERENCES workspace_stories(id),
 record TEXT NOT NULL CHECK(json_valid(record)),
 revision INTEGER NOT NULL DEFAULT 1
);
-- +goose StatementBegin
CREATE TRIGGER story_continuations_insert AFTER INSERT ON story_continuations BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id',s.id,'sessionId',s.id,'workspaceChanged',NEW.story_id)
 FROM sessions s JOIN app_settings a ON s.project_id=json_extract(a.coordination,'$.coordinatorProjectId');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER story_continuations_update AFTER UPDATE ON story_continuations BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id',s.id,'sessionId',s.id,'workspaceChanged',NEW.story_id)
 FROM sessions s JOIN app_settings a ON s.project_id=json_extract(a.coordination,'$.coordinatorProjectId');
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER story_continuations_update;
DROP TRIGGER story_continuations_insert;
DROP TABLE story_continuations;
