-- name: InsertTodo :one
INSERT INTO todos (id, workspace, title, priority, due_at, created_by)
VALUES (@id, @workspace, @title, @priority, @due_at, @created_by)
RETURNING *;

-- name: ListTodosByIDs :many
SELECT * FROM todos WHERE id = ANY(@ids::uuid[]);

-- name: LockTodo :one
SELECT * FROM todos WHERE id = @id AND workspace = @workspace FOR UPDATE;

-- name: UpdateTodo :one
UPDATE todos
SET title = @title, priority = @priority, done = @done, due_at = @due_at,
    updated_at = now(), version = version + 1
WHERE id = @id AND workspace = @workspace
RETURNING *;

-- name: DeleteTodo :execrows
DELETE FROM todos WHERE id = @id AND workspace = @workspace;

-- name: CountTodos :one
SELECT count(*) FILTER (WHERE NOT done) AS open, count(*) FILTER (WHERE done) AS done
FROM todos WHERE workspace = @workspace;

-- name: InsertAttachment :one
INSERT INTO attachments (todo_id, workspace, filename, content_type, size_bytes, content)
SELECT todos.id, todos.workspace, @filename, @content_type, @size_bytes, @content
FROM todos WHERE todos.id = @todo_id AND todos.workspace = @workspace
RETURNING id, todo_id, filename, content_type, size_bytes, created_at;

-- name: CountAttachments :one
SELECT count(*) FROM attachments WHERE workspace = @workspace;

-- name: InsertActivity :exec
INSERT INTO activity (id, workspace, todo_id, message)
VALUES (@id, @workspace, @todo_id, @message)
ON CONFLICT (id) DO NOTHING;

-- name: ListActivity :many
SELECT * FROM activity
WHERE workspace = @workspace AND (sqlc.narg(todo_id)::uuid IS NULL OR todo_id = sqlc.narg(todo_id))
ORDER BY created_at DESC, id DESC
LIMIT 100;
