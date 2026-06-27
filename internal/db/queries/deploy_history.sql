-- name: RecordDeploy :one
-- Append one event to an app's deploy timeline (image is the digest-pinned ref).
INSERT INTO deploy_history (app_namespace, app_name, build_name, image, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, occurred_at, app_namespace, app_name, build_name, image, status;

-- name: ListAppDeploys :many
-- The deploy timeline for one app, most-recent-first.
SELECT id, occurred_at, app_namespace, app_name, build_name, image, status
FROM deploy_history
WHERE app_namespace = $1 AND app_name = $2
ORDER BY id DESC
LIMIT $3 OFFSET $4;
