-- name: GetSessionAutoResume :one
SELECT policy, stop_reason FROM session_auto_resume WHERE session_id = ?;

-- name: UpsertAutoResumePolicy :exec
INSERT INTO session_auto_resume (session_id, policy, stop_reason, updated_at)
VALUES (?, ?, '', ?)
ON CONFLICT(session_id) DO UPDATE SET policy = excluded.policy, updated_at = excluded.updated_at;

-- name: UpsertStopReason :exec
INSERT INTO session_auto_resume (session_id, policy, stop_reason, updated_at)
VALUES (?, 'allowed', ?, ?)
ON CONFLICT(session_id) DO UPDATE SET stop_reason = excluded.stop_reason, updated_at = excluded.updated_at;

-- name: ClearStopReason :exec
UPDATE session_auto_resume SET stop_reason = '', updated_at = ? WHERE session_id = ? AND stop_reason <> '';

-- name: InsertDaemonRun :exec
-- A shutdown stamp may already have created this run's row (see InsertShutdownDaemonRun).
INSERT INTO daemon_runs (boot_id, started_at, heartbeat_at) VALUES (?, ?, ?)
ON CONFLICT(boot_id) DO NOTHING;

-- name: HeartbeatDaemonRun :exec
UPDATE daemon_runs SET heartbeat_at = ? WHERE boot_id = ?;

-- name: MarkDaemonShutdownStarted :execrows
UPDATE daemon_runs SET shutdown_started_at = sqlc.arg(shutdown_started_at), heartbeat_at = MAX(heartbeat_at, sqlc.arg(heartbeat_floor)) WHERE boot_id = sqlc.arg(boot_id) AND shutdown_started_at IS NULL;

-- name: InsertShutdownDaemonRun :exec
-- A run that shuts down before it recorded itself (it died or was stopped during the
-- boot health pass) still leaves a row, started now, so the next boot's shutdown window
-- starts after this run's own boot-pass exits and cannot re-capture them.
INSERT INTO daemon_runs (boot_id, started_at, heartbeat_at, shutdown_started_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(boot_id) DO NOTHING;

-- name: MarkDaemonEndedClean :exec
UPDATE daemon_runs SET ended_clean_at = ? WHERE boot_id = ?;

-- name: GetPreviousDaemonRun :one
SELECT boot_id, started_at, heartbeat_at, shutdown_started_at, ended_clean_at
FROM daemon_runs WHERE boot_id <> ? ORDER BY started_at DESC LIMIT 1;

-- name: CountDaemonRunsSince :one
SELECT COUNT(*) FROM daemon_runs WHERE started_at >= ?;

-- name: PruneDaemonRuns :exec
DELETE FROM daemon_runs WHERE boot_id NOT IN (SELECT boot_id FROM daemon_runs ORDER BY started_at DESC LIMIT ?);

-- name: GetResumeIntent :one
SELECT session_id, record, revision FROM session_resume_intents WHERE session_id = ?;

-- name: InsertResumeIntent :execrows
INSERT INTO session_resume_intents (session_id, record) VALUES (?, ?) ON CONFLICT(session_id) DO NOTHING;

-- name: UpdateResumeIntent :execrows
UPDATE session_resume_intents SET record = ?, revision = revision + 1 WHERE session_id = ? AND revision = ?;

-- name: DeleteResumeIntent :exec
DELETE FROM session_resume_intents WHERE session_id = ?;

-- name: ListResumeIntents :many
SELECT session_id, record, revision FROM session_resume_intents ORDER BY session_id;

-- name: ListInterruptedHumanTurnsForSession :many
-- Human-origin turns a restart cut off: still queued or running, or failed by the
-- controller-stop cleanup inside the shutdown window.
SELECT t.id, t.provider_turn_id
FROM conversation_turns t
WHERE t.handled_by_session_id = sqlc.arg(session_id)
  AND t.rolled_back_at IS NULL
  AND (
    t.state IN ('queued', 'running')
    OR (t.state = 'failed'
        AND t.error_message = 'controller ended before the turn completed'
        AND t.completed_at >= sqlc.arg(window_start)
        AND t.completed_at <= sqlc.arg(boot_start))
  )
  AND EXISTS (
    SELECT 1 FROM conversation_messages m
    WHERE m.turn_id = t.id AND m.role = 'user' AND m.origin = 'human'
  )
ORDER BY t.requested_at, t.id;
