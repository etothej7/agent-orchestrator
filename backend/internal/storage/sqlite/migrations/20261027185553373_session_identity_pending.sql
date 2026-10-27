-- +goose Up
-- +goose StatementBegin
-- Two-phase proof rotation (ED8 P2). A relaunch used to replace a session's proof before the new
-- process existed, so a failed launch left a live old process with a dead proof. Now the new proof
-- is PENDING beside the active one: both verify, the first use of the pending proof promotes it
-- (the old one stops verifying), and a launch that never happens leaves the old proof working.
-- confirmed_at is set when a proof was first presented (until then nothing can depend on it).
ALTER TABLE session_identity ADD COLUMN pending_sha256 TEXT
    CHECK (pending_sha256 IS NULL OR length(pending_sha256) = 64);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE session_identity ADD COLUMN confirmed_at TIMESTAMP;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX session_identity_pending_uq ON session_identity(pending_sha256)
    WHERE pending_sha256 IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE session_identity SET confirmed_at = issued_at WHERE confirmed_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX session_identity_pending_uq;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE session_identity DROP COLUMN confirmed_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE session_identity DROP COLUMN pending_sha256;
-- +goose StatementEnd
