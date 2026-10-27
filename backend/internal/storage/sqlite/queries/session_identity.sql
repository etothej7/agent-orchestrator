-- name: InsertSessionIdentity :exec
-- The first proof a session ever gets. Nothing depends on it yet, so it is active at once but not
-- confirmed.
INSERT INTO session_identity (session_id, proof_sha256, issued_at, generation)
VALUES (?, ?, ?, 1);

-- name: ReplaceUnconfirmedSessionIdentity :execrows
-- A new proof for a session whose current proof was never presented: no process can depend on the
-- old one, so it is replaced outright.
UPDATE session_identity
SET proof_sha256 = ?, issued_at = ?, generation = generation + 1, pending_sha256 = NULL
WHERE session_id = ? AND confirmed_at IS NULL;

-- name: SetPendingSessionIdentity :execrows
-- A new proof for a session whose current proof is in use: it waits beside the active one until its
-- first use, so a launch that fails leaves the old proof working.
UPDATE session_identity
SET pending_sha256 = ?, issued_at = ?
WHERE session_id = ? AND confirmed_at IS NOT NULL;

-- name: ConfirmSessionIdentity :exec
UPDATE session_identity SET confirmed_at = COALESCE(confirmed_at, ?) WHERE session_id = ?;

-- name: PromoteSessionIdentity :execrows
-- The pending proof was presented: the new process is up. It becomes the active proof and the old
-- one stops verifying.
UPDATE session_identity
SET proof_sha256 = pending_sha256, pending_sha256 = NULL, generation = generation + 1, confirmed_at = ?
WHERE session_id = ? AND pending_sha256 = ?;

-- name: GetSessionIdentityByProofHash :one
-- A proof verifies when it is the active one or the pending one.
SELECT session_id, proof_sha256, pending_sha256, issued_at, generation, confirmed_at
FROM session_identity WHERE proof_sha256 = ?1 OR pending_sha256 = ?1;

-- name: GetSessionIdentity :one
SELECT session_id, proof_sha256, pending_sha256, issued_at, generation, confirmed_at FROM session_identity WHERE session_id = ?;

-- name: DeleteSessionIdentity :exec
DELETE FROM session_identity WHERE session_id = ?;

-- name: ListSessionIdentitiesByIDs :many
-- Which of the given sessions hold a proof right now (one batched read for the session list).
SELECT session_id FROM session_identity WHERE session_id IN (sqlc.slice('session_ids'));
