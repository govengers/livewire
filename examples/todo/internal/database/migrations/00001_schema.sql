-- +goose Up
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

-- memberships puts every user in one workspace, the tenant of the API (D42).
CREATE TABLE memberships (
    user_id   uuid PRIMARY KEY,
    workspace text NOT NULL CHECK (workspace <> '')
);

-- api_keys holds the keys the CLI and the Go client authenticate with (D20), by their hash.
CREATE TABLE api_keys (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    key_hash   bytea NOT NULL UNIQUE,
    user_id    uuid NOT NULL REFERENCES memberships (user_id) ON DELETE CASCADE, -- a removed member's keys go with it
    username   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE todos (
    id         uuid PRIMARY KEY,
    workspace  text NOT NULL CHECK (workspace <> ''),
    title      text NOT NULL CHECK (title <> '' AND length(title) <= 200),
    priority   text NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high')),
    done       boolean NOT NULL DEFAULT false,
    due_at     timestamptz,
    created_by uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- version counts one todo's edits, far below 2^31
    -- squawk-ignore prefer-bigint-over-int
    version    integer NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE INDEX todos_workspace_created_at ON todos (workspace, created_at DESC, id);

CREATE TABLE attachments (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    todo_id      uuid NOT NULL REFERENCES todos (id) ON DELETE CASCADE,
    workspace    text NOT NULL CHECK (workspace <> ''),
    filename     text NOT NULL DEFAULT '',
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL CHECK (size_bytes >= 0),
    content      bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_todo_id ON attachments (todo_id);
CREATE INDEX attachments_workspace ON attachments (workspace);

-- activity is written by the completion job (govengers/jobs), not by a mutation.
CREATE TABLE activity (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace  text NOT NULL CHECK (workspace <> ''),
    todo_id    uuid NOT NULL,
    message    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX activity_workspace_created_at ON activity (workspace, created_at DESC, id);
