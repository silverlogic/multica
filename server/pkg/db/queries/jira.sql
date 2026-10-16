-- =====================
-- Jira Connection
-- =====================

-- name: ListJiraConnectionsByWorkspace :many
SELECT * FROM jira_connection
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetJiraConnectionByID :one
SELECT * FROM jira_connection
WHERE id = $1;

-- name: UpsertJiraConnection :one
-- Reconnecting the same site rotates the stored token/secret and identity in
-- place rather than creating a duplicate row (mirrors UpsertVCSConnection).
INSERT INTO jira_connection (
    workspace_id, base_url, account_email,
    api_token_encrypted, webhook_secret_encrypted, connected_by_id, jql
) VALUES (
    $1, $2, $3, $4, $5, sqlc.narg('connected_by_id'), sqlc.narg('jql')
)
ON CONFLICT (workspace_id, base_url) DO UPDATE SET
    account_email            = EXCLUDED.account_email,
    api_token_encrypted      = EXCLUDED.api_token_encrypted,
    webhook_secret_encrypted = EXCLUDED.webhook_secret_encrypted,
    connected_by_id          = EXCLUDED.connected_by_id,
    jql                      = EXCLUDED.jql,
    updated_at               = now()
RETURNING *;

-- name: DeleteJiraConnection :exec
-- These tables carry no FKs (project migration rules), so dependent cleanup
-- is done explicitly here, in one statement so it commits or rolls back
-- atomically with the connection row. The target CTE scopes the child delete
-- to a connection that actually belongs to the workspace, so a wrong
-- workspace_id is a no-op rather than deleting another tenant's link rows.
WITH target AS (
    SELECT jira_connection.id FROM jira_connection
    WHERE jira_connection.id = $1 AND jira_connection.workspace_id = $2
),
cleared_links AS (
    DELETE FROM jira_issue_link
    WHERE connection_id IN (SELECT target.id FROM target)
)
DELETE FROM jira_connection
WHERE jira_connection.id = $1 AND jira_connection.workspace_id = $2;

-- =====================
-- Jira issue link
-- =====================

-- name: GetJiraIssueLink :one
SELECT * FROM jira_issue_link
WHERE connection_id = $1 AND jira_issue_key = $2;

-- name: UpsertJiraIssueLink :one
-- One link per (connection, jira issue key). A webhook redelivery or repeat
-- event refreshes the sync bookkeeping in place; the multica_issue_id is
-- stable after the first insert (the Multica issue is created exactly once
-- per Jira issue).
-- jira_status / jira_priority record the values last seen from Jira; a NULL
-- argument (the event did not carry the field) keeps the stored value.
INSERT INTO jira_issue_link (
    workspace_id, connection_id, jira_issue_key, jira_issue_id,
    multica_issue_id, sync_status, last_inbound_at, jira_status, jira_priority
) VALUES (
    $1, $2, $3, $4, $5, $6, now(), sqlc.narg('jira_status'), sqlc.narg('jira_priority')
)
ON CONFLICT (connection_id, jira_issue_key) DO UPDATE SET
    jira_issue_id   = EXCLUDED.jira_issue_id,
    sync_status     = EXCLUDED.sync_status,
    jira_status     = COALESCE(EXCLUDED.jira_status, jira_issue_link.jira_status),
    jira_priority   = COALESCE(EXCLUDED.jira_priority, jira_issue_link.jira_priority),
    last_inbound_at = now(),
    updated_at      = now()
RETURNING *;

-- name: SyncIssueFromJira :one
-- Narrow inbound-sync write: only the fields Jira owns in PR 1 (title and
-- description). Deliberately NOT reusing UpdateIssue, whose non-COALESCE
-- sqlc.narg fields (assignee, dates, parent, project) would be nulled by a
-- partial update. workspace_id is the SQL-layer tenant guard.
-- revision / last_activity_at / updated_at move only when a mirrored field
-- actually changes, matching UpdateIssue, so a no-op redelivery does not look
-- like an edit to clients holding the current revision.
UPDATE issue AS i SET
    title       = $2,
    description = COALESCE(sqlc.narg('description'), i.description),
    revision    = i.revision + CASE WHEN changed.did_change THEN 1 ELSE 0 END,
    last_activity_at = CASE WHEN changed.did_change
        THEN GREATEST(COALESCE(i.last_activity_at, i.updated_at), now())
        ELSE i.last_activity_at
    END,
    updated_at  = CASE WHEN changed.did_change THEN now() ELSE i.updated_at END
FROM (
    SELECT cur.id,
           ROW(cur.title, cur.description) IS DISTINCT FROM
           ROW($2::text, COALESCE(sqlc.narg('description')::text, cur.description)) AS did_change
    FROM issue AS cur
    WHERE cur.id = $1 AND cur.workspace_id = $3
    FOR UPDATE
) AS changed
WHERE i.id = changed.id
RETURNING i.*;

-- name: SetJiraConnectionPropertyIDs :exec
UPDATE jira_connection SET
    key_property_id  = sqlc.narg('key_property_id'),
    link_property_id = sqlc.narg('link_property_id'),
    updated_at       = now()
WHERE id = $1 AND workspace_id = $2;

-- name: MoveIssueFromJira :one
-- Conditional status/priority write for inbound Jira sync. It lands only if
-- the issue still has the status and priority the sync read, so an edit made
-- in Multica meanwhile wins (the link keeps the old Jira values and the next
-- sync retries). A status move repositions to the top of the target column
-- and clears a duplicate mark, like UpdateIssueStatus; a priority-only write
-- keeps both.
UPDATE issue AS i SET
    status = sqlc.arg('target_status')::text,
    priority = sqlc.arg('target_priority')::text,
    duplicate_of_issue_id = CASE
        WHEN i.status <> sqlc.arg('target_status')::text THEN NULL
        ELSE i.duplicate_of_issue_id
    END,
    position = CASE
        WHEN i.status <> sqlc.arg('target_status')::text THEN (
            SELECT COALESCE(MIN(target.position), 0) - 1
            FROM issue AS target
            WHERE target.workspace_id = i.workspace_id
              AND target.status = sqlc.arg('target_status')::text
        )
        ELSE i.position
    END,
    revision = i.revision + 1,
    last_activity_at = GREATEST(COALESCE(i.last_activity_at, i.updated_at), now()),
    updated_at = now()
WHERE i.id = $1
  AND i.workspace_id = $2
  AND i.status = sqlc.arg('expected_status')::text
  AND i.priority = sqlc.arg('expected_priority')::text
  AND (i.status <> sqlc.arg('target_status')::text OR i.priority <> sqlc.arg('target_priority')::text)
RETURNING i.*;

-- name: ListJiraLinkedIssueIDsByKey :many
-- The Multica issues mirrored from a Jira key in a workspace, for resolving a
-- Jira key a pull request mentions. More than one row means two connected
-- Jira sites share the key, which callers treat as ambiguous.
SELECT DISTINCT multica_issue_id FROM jira_issue_link
WHERE workspace_id = $1 AND jira_issue_key = $2;
