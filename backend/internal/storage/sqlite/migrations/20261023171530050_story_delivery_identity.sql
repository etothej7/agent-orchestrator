-- +goose Up
ALTER TABLE reports ADD COLUMN recipient_session_id TEXT REFERENCES sessions(id);
DROP TRIGGER workspace_review_insert;
DROP TRIGGER workspace_review_update;
-- +goose StatementBegin
CREATE TRIGGER workspace_review_insert AFTER INSERT ON review_run
WHEN NEW.story_id IS NOT NULL
BEGIN
 INSERT INTO workspace_story_events(id, story_id, record, request)
 VALUES ('review-' || NEW.id, NEW.story_id,
 json_object('id','review-' || NEW.id,'storyId',NEW.story_id,'kind','native_review','senderSessionId','','reviewId',NEW.review_id,'reviewRunId',NEW.id,'recipientSessionId',NEW.session_id,'text',NEW.body,'reference',NEW.target_sha,'result',CASE WHEN NEW.status='complete' THEN NEW.verdict ELSE NEW.status END,'createdAt',strftime('%Y-%m-%dT%H:%M:%fZ',NEW.created_at)), '{}')
 ON CONFLICT(id) DO UPDATE SET record=excluded.record;
 UPDATE workspace_stories SET record=json_set(record, '$.status', 'in_progress', '$.revision', revision+1), revision=revision+1
 WHERE id=NEW.story_id AND json_extract(record, '$.status') IN ('ready_for_approval','done')
 AND (NOT (NEW.status='complete' AND NEW.verdict='approved') OR NEW.target_sha <> COALESCE((SELECT json_extract(record,'$.reference') FROM workspace_story_events WHERE story_id=NEW.story_id AND json_extract(record,'$.kind')='implementation' ORDER BY rowid DESC LIMIT 1),''));
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER workspace_review_update AFTER UPDATE ON review_run
WHEN NEW.story_id IS NOT NULL
BEGIN
 INSERT INTO workspace_story_events(id, story_id, record, request)
 VALUES ('review-' || NEW.id, NEW.story_id,
 json_object('id','review-' || NEW.id,'storyId',NEW.story_id,'kind','native_review','senderSessionId','','reviewId',NEW.review_id,'reviewRunId',NEW.id,'recipientSessionId',NEW.session_id,'text',NEW.body,'reference',NEW.target_sha,'result',CASE WHEN NEW.status='complete' THEN NEW.verdict ELSE NEW.status END,'createdAt',strftime('%Y-%m-%dT%H:%M:%fZ',NEW.created_at)), '{}')
 ON CONFLICT(id) DO UPDATE SET record=excluded.record;
 UPDATE workspace_stories SET record=json_set(record, '$.status', 'in_progress', '$.revision', revision+1), revision=revision+1
 WHERE id=NEW.story_id AND json_extract(record, '$.status') IN ('ready_for_approval','done')
 AND (NOT (NEW.status='complete' AND NEW.verdict='approved') OR NEW.target_sha <> COALESCE((SELECT json_extract(record,'$.reference') FROM workspace_story_events WHERE story_id=NEW.story_id AND json_extract(record,'$.kind')='implementation' ORDER BY rowid DESC LIMIT 1),''));
END;
-- +goose StatementEnd
UPDATE workspace_story_events SET record=json_set(record,'$.reviewId',json_extract(record,'$.senderSessionId'),'$.reviewRunId',substr(id,8),'$.senderSessionId','') WHERE json_extract(record,'$.kind')='native_review';
-- +goose Down
-- Durable recipient history is intentionally retained.
