-- +goose Up
-- +goose StatementBegin
-- Continuations ask whether a manager still has queued or running chat work on
-- every sweep; keep that lookup off a table scan. Only open turns are indexed.
CREATE INDEX IF NOT EXISTS idx_conversation_turns_open_by_session
    ON conversation_turns(handled_by_session_id) WHERE state IN ('queued', 'running');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_conversation_turns_open_by_session;
-- +goose StatementEnd
