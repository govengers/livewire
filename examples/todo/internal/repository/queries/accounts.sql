-- name: UpsertMembership :exec
INSERT INTO memberships (user_id, workspace) VALUES (@user_id, @workspace)
ON CONFLICT (user_id) DO UPDATE SET workspace = EXCLUDED.workspace;

-- name: MembershipWorkspace :one
SELECT workspace FROM memberships WHERE user_id = @user_id;

-- name: InsertAPIKey :one
INSERT INTO api_keys (key_hash, user_id, username) VALUES (@key_hash, @user_id, @username)
RETURNING id, created_at;

-- name: APIKeyByHash :one
SELECT api_keys.user_id, api_keys.username, memberships.workspace
FROM api_keys JOIN memberships ON memberships.user_id = api_keys.user_id
WHERE api_keys.key_hash = @key_hash;
