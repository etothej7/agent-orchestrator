-- name: UpsertSessionIdentity :exec
-- Issue or replace a session's proof hash. A replacement bumps the generation, so the old proof
-- stops verifying at once (the UNIQUE hash column means a hash can never belong to two sessions).
INSERT INTO session_identity (session_id, proof_sha256, issued_at, generation)
VALUES (?, ?, ?, 1)
ON CONFLICT(session_id) DO UPDATE SET
    proof_sha256 = excluded.proof_sha256,
    issued_at    = excluded.issued_at,
    generation   = session_identity.generation + 1;

-- name: GetSessionIdentityByProofHash :one
SELECT session_id, proof_sha256, issued_at, generation FROM session_identity WHERE proof_sha256 = ?;

-- name: GetSessionIdentity :one
SELECT session_id, proof_sha256, issued_at, generation FROM session_identity WHERE session_id = ?;

-- name: DeleteSessionIdentity :exec
DELETE FROM session_identity WHERE session_id = ?;
