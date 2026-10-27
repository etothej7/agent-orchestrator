-- name: UpsertReviewIdentity :exec
-- Issue or replace a reviewer's proof hash. A replacement bumps the generation, so the old proof
-- stops verifying at once.
INSERT INTO review_identity (review_id, proof_sha256, issued_at, generation)
VALUES (?, ?, ?, 1)
ON CONFLICT(review_id) DO UPDATE SET
    proof_sha256 = excluded.proof_sha256,
    issued_at    = excluded.issued_at,
    generation   = review_identity.generation + 1;

-- name: GetReviewIdentityByProofHash :one
SELECT review_id, proof_sha256, issued_at, generation FROM review_identity WHERE proof_sha256 = ?;

-- name: GetReviewIdentity :one
SELECT review_id, proof_sha256, issued_at, generation FROM review_identity WHERE review_id = ?;

-- name: DeleteReviewIdentity :exec
DELETE FROM review_identity WHERE review_id = ?;
