-- +goose Up
-- +goose StatementBegin
-- Per-session identity proofs (ED8). The daemon issues a random proof when a session is
-- launched or relaunched and keeps ONLY its hash: the proof itself lives in the session's
-- environment and nowhere else, so the database (readable by the same OS user) cannot be used
-- to mint a session's identity. One proof per session; a relaunch replaces it (generation
-- counts the replacements), and a deleted session takes its proof with it.
CREATE TABLE session_identity (
    session_id   TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    proof_sha256 TEXT NOT NULL UNIQUE CHECK (length(proof_sha256) = 64),
    issued_at    TIMESTAMP NOT NULL,
    generation   INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE session_identity;
-- +goose StatementEnd
