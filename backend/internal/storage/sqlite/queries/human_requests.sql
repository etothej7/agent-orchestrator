-- name: InsertHumanRequest :execrows
-- A retried filing finds the open request with the same (kind, session, payload hash) instead.
INSERT INTO human_requests (id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
ON CONFLICT DO NOTHING;

-- name: GetHumanRequest :one
SELECT id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at
FROM human_requests WHERE id = ?;

-- name: GetOpenHumanRequest :one
SELECT id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at
FROM human_requests WHERE kind = ? AND session_id = ? AND payload_hash = ? AND state = 'pending';

-- name: ListHumanRequestsByState :many
SELECT id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at
FROM human_requests WHERE state = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: ListRecentHumanRequests :many
SELECT id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at
FROM human_requests ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: CountPendingHumanRequestsByRequester :one
SELECT COUNT(*) FROM human_requests WHERE state = 'pending' AND requester_kind = ? AND requester_session_id = ?;

-- name: CountHumanRequestsFiledSince :one
SELECT COUNT(*) FROM human_requests WHERE requester_kind = ? AND requester_session_id = ? AND created_at >= ?;

-- name: ClaimHumanRequest :execrows
-- Approval claims the request atomically: only a pending, unexpired request can be claimed, once.
UPDATE human_requests SET state = 'approved', decided_at = sqlc.arg(decided_at)
WHERE id = sqlc.arg(id) AND state = 'pending' AND expires_at > sqlc.arg(decided_at);

-- name: DenyHumanRequest :execrows
UPDATE human_requests SET state = 'denied', decided_at = sqlc.arg(decided_at)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: FinishHumanRequest :execrows
UPDATE human_requests SET state = sqlc.arg(state), result = sqlc.arg(result), decided_at = COALESCE(decided_at, sqlc.arg(decided_at))
WHERE id = sqlc.arg(id) AND state = 'approved';

-- name: ExpireHumanRequests :many
-- Pending requests past their deadline, and approvals that never finished (the daemon stopped
-- between claiming and applying), become expired / failed. The rows come back so each requester is told.
UPDATE human_requests
SET state = CASE WHEN state = 'pending' THEN 'expired' ELSE 'failed' END,
    result = CASE WHEN state = 'pending' THEN 'expired before anyone decided' ELSE 'interrupted before it was applied; ask again' END,
    decided_at = COALESCE(decided_at, sqlc.arg(now))
WHERE (state = 'pending' AND expires_at <= sqlc.arg(now))
   OR (state = 'approved' AND decided_at <= sqlc.arg(stale_before))
RETURNING id, kind, session_id, requester_kind, requester_session_id, payload, payload_hash, summary, changes, state, result, created_at, expires_at, decided_at;
