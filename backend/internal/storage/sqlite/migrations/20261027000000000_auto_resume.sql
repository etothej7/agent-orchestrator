-- +goose Up
-- +goose StatementBegin
-- Auto-resume after a restart (ED7). Three small tables instead of new columns on
-- the heavily queried sessions table:
--  * session_auto_resume: the per-session policy (default allowed) and why an
--    agent was stopped on purpose. 'never' protects a session from auto-resume,
--    replacement and teardown. A session with no row is 'allowed'.
--  * daemon_runs: a liveness stamp per daemon run (heartbeat, shutdown start, clean
--    end), which is also the boot log used to bound resume storms.
--  * session_resume_intents: one durable record per session captured at boot.
CREATE TABLE session_auto_resume (
    session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    policy      TEXT NOT NULL DEFAULT 'allowed' CHECK (policy IN ('allowed', 'never')),
    stop_reason TEXT NOT NULL DEFAULT '' CHECK (stop_reason IN ('', 'user_stop', 'account_stop', 'update_stop')),
    updated_at  TIMESTAMP NOT NULL
);

CREATE TABLE daemon_runs (
    boot_id             TEXT PRIMARY KEY,
    started_at          TIMESTAMP NOT NULL,
    heartbeat_at        TIMESTAMP NOT NULL,
    shutdown_started_at TIMESTAMP,
    ended_clean_at      TIMESTAMP
);
CREATE INDEX idx_daemon_runs_started ON daemon_runs(started_at);

CREATE TABLE session_resume_intents (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    record     TEXT NOT NULL CHECK (json_valid(record)),
    revision   INTEGER NOT NULL DEFAULT 1
);

-- Personal build seed: the two orchestrators Ed never approved for restart are
-- protected from the first boot of this version. A no-op where they do not exist.
INSERT INTO session_auto_resume (session_id, policy, updated_at)
SELECT id, 'never', CURRENT_TIMESTAMP FROM sessions
WHERE id IN ('optimus_db-1', 'optimus_rails_app-1');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE session_resume_intents;
DROP INDEX idx_daemon_runs_started;
DROP TABLE daemon_runs;
DROP TABLE session_auto_resume;
-- +goose StatementEnd
