-- +goose Up
-- Correlates an accepted plan to one real human request. Stories stay canonical.
CREATE TABLE story_intakes (
 id TEXT PRIMARY KEY,
 source_message_id TEXT NOT NULL UNIQUE,
 sender_session_id TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 epic_id TEXT NOT NULL REFERENCES workspace_epics(id)
);
-- +goose Down
DROP TABLE story_intakes;
