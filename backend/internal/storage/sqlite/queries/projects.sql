-- name: UpsertProject :exec
INSERT INTO projects (id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    path = excluded.path,
    repo_origin_url = excluded.repo_origin_url,
    display_name = excluded.display_name,
    archived_at = excluded.archived_at,
    -- agentMayMerge is the person's setting and is changed only by SetProjectAgentMayMerge: whatever a writer
    -- carries for it (a stale copy it read earlier, or an agent's rewrite) is replaced by the stored value, in
    -- the same statement that writes the rest, so no interleaving can flip it.
    config = CASE
        WHEN json_valid(projects.config) AND json_extract(projects.config, '$.agentMayMerge') = 1
            THEN json_set(CASE WHEN json_valid(excluded.config) THEN excluded.config ELSE '{}' END, '$.agentMayMerge', json('true'))
        WHEN json_valid(excluded.config) THEN json_remove(excluded.config, '$.agentMayMerge')
        ELSE excluded.config
    END,
    kind = excluded.kind;

-- name: UpsertImportedProject :exec
INSERT INTO projects (id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    path = excluded.path,
    repo_origin_url = excluded.repo_origin_url,
    display_name = excluded.display_name,
    registered_at = excluded.registered_at,
    archived_at = excluded.archived_at,
    -- agentMayMerge is the person's setting and is changed only by SetProjectAgentMayMerge: whatever a writer
    -- carries for it (a stale copy it read earlier, or an agent's rewrite) is replaced by the stored value, in
    -- the same statement that writes the rest, so no interleaving can flip it.
    config = CASE
        WHEN json_valid(projects.config) AND json_extract(projects.config, '$.agentMayMerge') = 1
            THEN json_set(CASE WHEN json_valid(excluded.config) THEN excluded.config ELSE '{}' END, '$.agentMayMerge', json('true'))
        WHEN json_valid(excluded.config) THEN json_remove(excluded.config, '$.agentMayMerge')
        ELSE excluded.config
    END,
    kind = excluded.kind;

-- name: GetProject :one
SELECT id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind
FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind
FROM projects WHERE archived_at IS NULL ORDER BY id;

-- name: CountProjectsIncludingArchived :one
SELECT COUNT(*) FROM projects;

-- name: FindProjectByPath :one
SELECT id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind
FROM projects WHERE path = ? AND archived_at IS NULL;

-- name: UpdateProjectSettings :execrows
UPDATE projects
SET display_name = sqlc.arg(display_name),
    -- agentMayMerge is kept from the stored row (see UpsertProject); only SetProjectAgentMayMerge changes it.
    config = CASE
        WHEN json_valid(projects.config) AND json_extract(projects.config, '$.agentMayMerge') = 1
            THEN json_set(CASE WHEN json_valid(sqlc.arg(config)) THEN sqlc.arg(config) ELSE '{}' END, '$.agentMayMerge', json('true'))
        WHEN json_valid(sqlc.arg(config)) THEN json_remove(sqlc.arg(config), '$.agentMayMerge')
        ELSE sqlc.arg(config)
    END
WHERE id = sqlc.arg(id) AND archived_at IS NULL;

-- name: SetProjectAgentMayMerge :execrows
-- The only statement that changes whether agents may merge. The caller must be the person (the service checks).
UPDATE projects
SET config = CASE
    WHEN CAST(sqlc.arg(enabled) AS INTEGER) = 1 THEN json_set(CASE WHEN json_valid(config) THEN config ELSE '{}' END, '$.agentMayMerge', json('true'))
    ELSE json_remove(CASE WHEN json_valid(config) THEN config ELSE '{}' END, '$.agentMayMerge')
END
WHERE id = sqlc.arg(id) AND archived_at IS NULL;

-- name: ArchiveProject :execrows
UPDATE projects SET archived_at = ? WHERE id = ? AND archived_at IS NULL;

-- name: PinProjectSessionPermissions :exec
UPDATE sessions SET session_permissions = sqlc.arg(permissions)
WHERE project_id = sqlc.arg(project_id) AND kind = sqlc.arg(kind)
  AND LENGTH(session_permissions) = 0;
