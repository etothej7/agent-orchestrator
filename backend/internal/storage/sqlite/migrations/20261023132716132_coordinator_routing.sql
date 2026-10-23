-- +goose Up
ALTER TABLE app_settings ADD COLUMN coordination TEXT NOT NULL DEFAULT '{}';
ALTER TABLE reports ADD COLUMN recipient_project_id TEXT REFERENCES projects(id);
CREATE INDEX reports_recipient_pending ON reports(COALESCE(recipient_project_id, project_id), delivery_state, available_at);

-- +goose Down
DROP INDEX reports_recipient_pending;
ALTER TABLE reports DROP COLUMN recipient_project_id;
ALTER TABLE app_settings DROP COLUMN coordination;
