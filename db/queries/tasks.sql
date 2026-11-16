-- Every query on tasks skips deleted rows (deleted_at IS NULL), or its name ends in
-- IncludingDeleted; make lint (sqlc vet) refuses any other. See docs/standards/database.md.

-- name: CreateTask :one
INSERT INTO tasks (title)
VALUES ($1)
RETURNING *;

-- name: GetTask :one
SELECT *
FROM tasks
WHERE id = $1
  AND deleted_at IS NULL;

-- name: ListTasks :many
SELECT *
FROM tasks
WHERE deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $1;

-- ListTasksAfter returns the page after the cursor row (created_at, id), newest first.
-- name: ListTasksAfter :many
SELECT *
FROM tasks
WHERE (created_at, id) < (sqlc.arg('created_at')::timestamptz, sqlc.arg('id')::uuid)
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- A NULL argument leaves that column as it is.
-- name: UpdateTask :one
UPDATE tasks
SET title = COALESCE(sqlc.narg('title'), title),
    done = COALESCE(sqlc.narg('done'), done),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
RETURNING *;

-- name: DeleteTask :execrows
UPDATE tasks
SET deleted_at = now()
WHERE id = $1
  AND deleted_at IS NULL;

-- RestoreTask brings back a deleted task, changed now: the purge ages a done task by its last
-- change, so it doesn't remove one the moment it's restored.
-- name: RestoreTask :one
UPDATE tasks
SET deleted_at = NULL,
    updated_at = now()
WHERE id = $1
  AND deleted_at IS NOT NULL
RETURNING *;

-- Removes for good the tasks deleted more than older_than_days ago, and the live tasks marked done
-- whose last change is more than older_than_days ago.
-- name: PurgeDoneTasks :execrows
DELETE FROM tasks
WHERE deleted_at < now() - sqlc.arg('older_than_days')::bigint * interval '1 day'
   OR (deleted_at IS NULL
       AND done
       AND updated_at < now() - sqlc.arg('older_than_days')::bigint * interval '1 day');
