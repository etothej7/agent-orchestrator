-- name: CreateWorkspaceEpic :exec
INSERT INTO workspace_epics(id, title) VALUES (?, ?);
-- name: ListWorkspaceEpics :many
SELECT * FROM workspace_epics ORDER BY rowid;
-- name: CreateWorkspaceStory :exec
INSERT INTO workspace_stories(id, epic_id, record, revision) VALUES (?, ?, ?, 1);
-- name: ListWorkspaceStories :many
SELECT * FROM workspace_stories ORDER BY rowid;
-- name: GetWorkspaceStory :one
SELECT * FROM workspace_stories WHERE id = ?;
-- name: UpdateWorkspaceStory :execrows
UPDATE workspace_stories SET record = ?, revision = revision + 1 WHERE id = ? AND revision = ?;
-- name: CreateWorkspaceEvent :exec
INSERT INTO workspace_story_events(id, story_id, record, request) VALUES (?, ?, ?, ?);
-- name: GetWorkspaceEvent :one
SELECT * FROM workspace_story_events WHERE id = ?;
-- name: ListWorkspaceEvents :many
SELECT * FROM workspace_story_events WHERE story_id = ? ORDER BY rowid;
-- name: UpdateWorkspaceEvent :exec
UPDATE workspace_story_events SET record = ? WHERE id = ?;
-- name: GetStoryReportByClientID :one
SELECT * FROM reports WHERE session_id = ? AND client_report_id = ?;

-- name: ListWorkspaceReplyMessages :many
-- Only the exact outbound turn; never session-wide history or activity payloads.
-- Receipts, legacy steer activities, and promotions all carry shared-turn facts.
-- An unresolved promotion has no durable target, so withhold conversation-wide.
WITH exchanges AS (
 SELECT e.id, CAST(json_extract(e.record, '$.createdAt') AS TEXT) AS position, e.record,
        t.id AS turn_id, t.conversation_id,
        CASE WHEN json_extract(e.record, '$.deliveryKind') = 'turn'
          AND t.id IS NOT NULL AND u.id IS NOT NULL
          AND NOT EXISTS (
            SELECT 1 FROM conversation_messages other
            WHERE other.conversation_id = t.conversation_id AND other.turn_id = t.id
              AND other.role = 'user' AND other.id <> u.id
          )
          AND NOT EXISTS (
            SELECT 1 FROM conversation_steer_deliveries d
            WHERE d.conversation_id = t.conversation_id AND d.state <> 'rejected'
              AND (d.state = 'reserved' OR d.provider_turn_id = t.provider_turn_id)
          )
          AND NOT EXISTS (
            SELECT 1 FROM conversation_activities a
            WHERE a.conversation_id = t.conversation_id AND a.turn_id = t.id
              AND a.kind = 'system' AND json_extract(a.detail_json, '$.event') = 'steer'
          )
          AND NOT EXISTS (
            SELECT 1 FROM conversation_turns source
            WHERE source.conversation_id = t.conversation_id
              AND (source.promoted_to_turn_id = t.id OR source.promotion_started_at IS NOT NULL)
          )
          THEN 'turn' ELSE 'unavailable' END AS attribution
 FROM workspace_story_events e
 LEFT JOIN conversation_turns t
   ON json_extract(e.record, '$.deliveryKind') = 'turn'
   AND t.id = json_extract(e.record, '$.reference')
   AND t.handled_by_session_id = json_extract(e.record, '$.recipientSessionId')
 LEFT JOIN conversation_messages u
   ON u.conversation_id = t.conversation_id AND u.turn_id = t.id AND u.role = 'user'
   AND u.client_message_id = 'story-' || e.id
   AND u.sender_session_id = json_extract(e.record, '$.senderSessionId')
 WHERE e.story_id = ? AND json_extract(e.record, '$.result') = 'delivered'
   AND json_extract(e.record, '$.deliveryKind') IN ('turn', 'steer')
)
SELECT e.id AS exchange_id, e.attribution,
       CAST(json_extract(e.record, '$.recipientSessionId') AS TEXT) AS sender_session_id,
       CAST(json_extract(e.record, '$.senderSessionId') AS TEXT) AS recipient_session_id,
       m.id, m.conversation_id, m.turn_id, m.text, m.streaming, m.created_at, m.updated_at
FROM exchanges e
LEFT JOIN conversation_messages m ON e.attribution = 'turn'
 AND m.conversation_id = e.conversation_id AND m.turn_id = e.turn_id
 AND m.role = 'assistant' AND m.origin = 'provider'
ORDER BY e.position, e.id, m.sequence;
