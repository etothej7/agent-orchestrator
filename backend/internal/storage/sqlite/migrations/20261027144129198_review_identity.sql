-- +goose Up
-- +goose StatementBegin
-- Identity proofs for native reviewers (ED8-1 F1). A reviewer is not a row of `sessions`: it is a
-- `review` row (the pane or chat that reviews one worker's pull request), so it cannot hold a
-- session_identity proof. Same rules as session_identity: the daemon keeps ONLY the proof's hash,
-- one proof per reviewer, a relaunch replaces it (generation counts replacements), and deleting
-- the review takes its proof with it.
CREATE TABLE review_identity (
    review_id    TEXT PRIMARY KEY REFERENCES review(id) ON DELETE CASCADE,
    proof_sha256 TEXT NOT NULL UNIQUE CHECK (length(proof_sha256) = 64),
    issued_at    TIMESTAMP NOT NULL,
    generation   INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE review_identity;
-- +goose StatementEnd
