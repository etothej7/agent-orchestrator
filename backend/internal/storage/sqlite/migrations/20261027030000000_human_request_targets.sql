-- +goose Up
-- +goose StatementBegin
-- A pending request is about one thing: a session, or (new) a project. Exactly one of session_id
-- and project_id is set, and each goes with its row. SQLite cannot relax NOT NULL in place, so the
-- table is rebuilt; nothing references it. The open-request uniqueness now covers either target.
CREATE TABLE human_requests_new (
    id                   TEXT PRIMARY KEY,
    kind                 TEXT NOT NULL,
    session_id           TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    project_id           TEXT REFERENCES projects(id) ON DELETE CASCADE,
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
    decided_at           TIMESTAMP,
    CHECK ((session_id IS NOT NULL) <> (project_id IS NOT NULL))
);
INSERT INTO human_requests_new (id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at)
SELECT id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at FROM human_requests;
DROP TABLE human_requests;
ALTER TABLE human_requests_new RENAME TO human_requests;
CREATE INDEX idx_human_requests_state ON human_requests(state, created_at);
CREATE INDEX idx_human_requests_requester ON human_requests(requester_session_id, created_at);
-- One open request per (kind, target, payload): a retried filing returns the same request. The target key is
-- the session id, or "project:" and the project id (a session id has no colon).
CREATE UNIQUE INDEX idx_human_requests_open ON human_requests(kind, COALESCE(session_id, 'project:' || project_id), payload_hash) WHERE state = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Project-targeted requests have no session to hang on to, so they are dropped on the way down.
DELETE FROM human_requests WHERE session_id IS NULL;
CREATE TABLE human_requests_old (
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
INSERT INTO human_requests_old (id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at)
SELECT id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at FROM human_requests;
DROP TABLE human_requests;
ALTER TABLE human_requests_old RENAME TO human_requests;
CREATE INDEX idx_human_requests_state ON human_requests(state, created_at);
CREATE INDEX idx_human_requests_requester ON human_requests(requester_session_id, created_at);
CREATE UNIQUE INDEX idx_human_requests_open ON human_requests(kind, session_id, payload_hash) WHERE state = 'pending';
-- +goose StatementEnd
