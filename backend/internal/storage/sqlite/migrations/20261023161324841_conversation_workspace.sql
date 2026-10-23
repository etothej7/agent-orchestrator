-- +goose Up
CREATE TABLE workspace_epics (
 id TEXT PRIMARY KEY,
 title TEXT NOT NULL CHECK(length(trim(title)) > 0)
);
CREATE TABLE workspace_stories (
 id TEXT PRIMARY KEY,
 epic_id TEXT NOT NULL REFERENCES workspace_epics(id),
 record TEXT NOT NULL CHECK(json_valid(record)),
 revision INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX workspace_stories_epic ON workspace_stories(epic_id);
CREATE TABLE workspace_story_events (
 id TEXT PRIMARY KEY,
 story_id TEXT NOT NULL REFERENCES workspace_stories(id),
 record TEXT NOT NULL CHECK(json_valid(record)),
 request TEXT NOT NULL CHECK(json_valid(request))
);
CREATE INDEX workspace_story_events_story ON workspace_story_events(story_id);
ALTER TABLE reports ADD COLUMN story_id TEXT REFERENCES workspace_stories(id);
ALTER TABLE reports ADD COLUMN client_report_id TEXT;
CREATE UNIQUE INDEX reports_client_story ON reports(session_id, client_report_id) WHERE client_report_id IS NOT NULL;
-- +goose StatementBegin
CREATE TRIGGER workspace_stories_insert AFTER INSERT ON workspace_stories BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id', s.id, 'sessionId', s.id, 'workspaceChanged', NEW.id)
 FROM sessions s JOIN app_settings a ON s.project_id = json_extract(a.coordination, '$.coordinatorProjectId');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER workspace_stories_update AFTER UPDATE ON workspace_stories BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id', s.id, 'sessionId', s.id, 'workspaceChanged', NEW.id)
 FROM sessions s JOIN app_settings a ON s.project_id = json_extract(a.coordination, '$.coordinatorProjectId');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER workspace_events_insert AFTER INSERT ON workspace_story_events BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id', s.id, 'sessionId', s.id, 'workspaceChanged', NEW.story_id)
 FROM sessions s JOIN app_settings a ON s.project_id = json_extract(a.coordination, '$.coordinatorProjectId');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER workspace_events_update AFTER UPDATE ON workspace_story_events BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id', s.id, 'sessionId', s.id, 'workspaceChanged', NEW.story_id)
 FROM sessions s JOIN app_settings a ON s.project_id = json_extract(a.coordination, '$.coordinatorProjectId');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER workspace_epics_insert AFTER INSERT ON workspace_epics BEGIN
 INSERT INTO change_log(project_id, session_id, event_type, payload)
 SELECT s.project_id, s.id, 'session_updated', json_object('id', s.id, 'sessionId', s.id, 'workspaceChanged', NEW.id)
 FROM sessions s JOIN app_settings a ON s.project_id = json_extract(a.coordination, '$.coordinatorProjectId');
END;
-- +goose StatementEnd
ALTER TABLE review_run ADD COLUMN story_id TEXT REFERENCES workspace_stories(id);
-- +goose StatementBegin
CREATE TRIGGER workspace_review_insert AFTER INSERT ON review_run
WHEN NEW.story_id IS NOT NULL
BEGIN
 INSERT INTO workspace_story_events(id, story_id, record, request)
 VALUES ('review-' || NEW.id, NEW.story_id,
 json_object('id','review-' || NEW.id,'storyId',NEW.story_id,'kind','native_review','senderSessionId',NEW.review_id,'recipientSessionId',NEW.session_id,'text',NEW.body,'reference',NEW.target_sha,'result',CASE WHEN NEW.status='complete' THEN NEW.verdict ELSE NEW.status END,'createdAt',strftime('%Y-%m-%dT%H:%M:%fZ',NEW.created_at)), '{}')
 ON CONFLICT(id) DO UPDATE SET record=excluded.record;
 UPDATE workspace_stories SET record=json_set(record, '$.status', 'in_progress', '$.revision', revision+1), revision=revision+1
 WHERE id=NEW.story_id AND json_extract(record, '$.status') IN ('ready_for_approval','done')
 AND NOT (NEW.status='complete' AND NEW.verdict='approved');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER workspace_review_update AFTER UPDATE ON review_run
WHEN NEW.story_id IS NOT NULL
BEGIN
 INSERT INTO workspace_story_events(id, story_id, record, request)
 VALUES ('review-' || NEW.id, NEW.story_id,
 json_object('id','review-' || NEW.id,'storyId',NEW.story_id,'kind','native_review','senderSessionId',NEW.review_id,'recipientSessionId',NEW.session_id,'text',NEW.body,'reference',NEW.target_sha,'result',CASE WHEN NEW.status='complete' THEN NEW.verdict ELSE NEW.status END,'createdAt',strftime('%Y-%m-%dT%H:%M:%fZ',NEW.created_at)), '{}')
 ON CONFLICT(id) DO UPDATE SET record=excluded.record;
 UPDATE workspace_stories SET record=json_set(record, '$.status', 'in_progress', '$.revision', revision+1), revision=revision+1
 WHERE id=NEW.story_id AND json_extract(record, '$.status') IN ('ready_for_approval','done')
 AND NOT (NEW.status='complete' AND NEW.verdict='approved');
END;
-- +goose StatementEnd
-- +goose Down
-- Preserve durable history, but release dependencies before older schemas downgrade.
DROP TRIGGER IF EXISTS workspace_stories_insert;
DROP TRIGGER IF EXISTS workspace_stories_update;
DROP TRIGGER IF EXISTS workspace_events_insert;
DROP TRIGGER IF EXISTS workspace_events_update;
DROP TRIGGER IF EXISTS workspace_epics_insert;
DROP TRIGGER IF EXISTS workspace_review_insert;
DROP TRIGGER IF EXISTS workspace_review_update;
