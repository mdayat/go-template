-- name: CreateTask :one
INSERT INTO tasks (title)
VALUES ($1)
RETURNING *;

-- name: GetTask :one
SELECT *
FROM tasks
WHERE id = $1;

-- name: ListTasks :many
SELECT *
FROM tasks
ORDER BY created_at DESC, id DESC
LIMIT $1;

-- ListTasksAfter returns the page after the cursor row (created_at, id), newest first.
-- name: ListTasksAfter :many
SELECT *
FROM tasks
WHERE (created_at, id) < (sqlc.arg('created_at')::timestamptz, sqlc.arg('id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- A NULL argument leaves that column as it is.
-- name: UpdateTask :one
UPDATE tasks
SET title = COALESCE(sqlc.narg('title'), title),
    done = COALESCE(sqlc.narg('done'), done),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteTask :execrows
DELETE FROM tasks
WHERE id = $1;

-- Deletes tasks marked done whose last change is more than older_than_days ago.
-- name: PurgeDoneTasks :execrows
DELETE FROM tasks
WHERE done
  AND updated_at < now() - sqlc.arg('older_than_days')::bigint * interval '1 day';
