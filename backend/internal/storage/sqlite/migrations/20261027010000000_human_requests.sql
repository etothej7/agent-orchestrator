-- +goose Up
-- +goose StatementBegin
-- Pending requests (ED8): a human-only action that an agent or a terminal asked for and
-- the person confirms in the app. The daemon renders the summary and the changes from the
-- structured payload when the request is filed, so the card never shows requester text;
-- payload_hash binds an approval to exactly what was shown. kind is validated in code (the
-- list grows), state is a closed set.
CREATE TABLE human_requests (
    id                   TEXT PRIMARY KEY,
    kind                 TEXT NOT NULL,
    session_id           TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    requester_kind       TEXT NOT NULL CHECK (requester_kind IN ('session', 'terminal')),
    requester_session_id TEXT NOT NULL DEFAULT '',
    payload              TEXT NOT NULL CHECK (json_valid(payload)),
    payload_hash         TEXT NOT NULL,
    summary              TEXT NOT NULL,
    changes              TEXT NOT NULL CHECK (json_valid(changes)),
    state                TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'applied', 'failed')),
    result               TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMP NOT NULL,
    expires_at           TIMESTAMP NOT NULL,
    decided_at           TIMESTAMP
);
CREATE INDEX idx_human_requests_state ON human_requests(state, created_at);
CREATE INDEX idx_human_requests_requester ON human_requests(requester_session_id, created_at);
-- One open request per (kind, target, payload): a retried filing returns the same request.
CREATE UNIQUE INDEX idx_human_requests_open ON human_requests(kind, session_id, payload_hash) WHERE state = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_human_requests_open;
DROP INDEX idx_human_requests_requester;
DROP INDEX idx_human_requests_state;
DROP TABLE human_requests;
-- +goose StatementEnd
