-- Every query on tasks skips deleted rows (deleted_at IS NULL), or its name ends in
-- IncludingDeleted; make lint (sqlc vet) refuses any other. See docs/standards/database.md.
--
-- owner_id limits a query to one user's tasks; NULL means an admin's: everyone's.

-- name: CreateTask :one
INSERT INTO tasks (title, owner_id)
VALUES ($1, $2)
RETURNING *;

-- name: GetTask :one
SELECT *
FROM tasks
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
  AND (sqlc.narg('owner_id')::uuid IS NULL OR owner_id = sqlc.narg('owner_id'));

-- ListOwnerTasks returns one user's tasks, newest first.
-- name: ListOwnerTasks :many
SELECT *
FROM tasks
WHERE owner_id = $1
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- ListOwnerTasksAfter returns the page of one user's tasks after the cursor row (created_at, id).
-- name: ListOwnerTasksAfter :many
SELECT *
FROM tasks
WHERE owner_id = sqlc.arg('owner_id')
  AND (created_at, id) < (sqlc.arg('created_at')::timestamptz, sqlc.arg('id')::uuid)
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- ListTasks returns everyone's tasks, newest first, for admins. Not ListOwnerTasks with an
-- optional owner: Postgres may then settle on one plan for both and stop using the owner index,
-- slowing every user's list as the table grows.
-- name: ListTasks :many
SELECT *
FROM tasks
WHERE deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $1;

-- ListTasksAfter returns the page of everyone's tasks after the cursor row (created_at, id).
-- name: ListTasksAfter :many
SELECT *
FROM tasks
WHERE (created_at, id) < (sqlc.arg('created_at')::timestamptz, sqlc.arg('id')::uuid)
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- A NULL title or done leaves that column as it is.
-- name: UpdateTask :one
UPDATE tasks
SET title = COALESCE(sqlc.narg('title'), title),
    done = COALESCE(sqlc.narg('done'), done),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
  AND (sqlc.narg('owner_id')::uuid IS NULL OR owner_id = sqlc.narg('owner_id'))
RETURNING *;

-- name: DeleteTask :execrows
UPDATE tasks
SET deleted_at = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
  AND (sqlc.narg('owner_id')::uuid IS NULL OR owner_id = sqlc.narg('owner_id'));

-- RestoreTask brings back a deleted task. A restore is a change, so it sets updated_at.
-- name: RestoreTask :one
UPDATE tasks
SET deleted_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NOT NULL
  AND (sqlc.narg('owner_id')::uuid IS NULL OR owner_id = sqlc.narg('owner_id'))
RETURNING *;

-- Removes for good the tasks deleted more than older_than_days ago, whoever owns them.
-- name: PurgeDeletedTasks :execrows
DELETE FROM tasks
WHERE deleted_at < now() - sqlc.arg('older_than_days')::bigint * interval '1 day';

-- ListTasksByIDAfter pages through every live task in ID order, for one-off commands that check
-- each one: an ID's order never changes, so a page ends where the next begins even when nothing
-- was changed (a dry run).
-- name: ListTasksByIDAfter :many
SELECT id, owner_id, title
FROM tasks
WHERE id > sqlc.arg('after')::uuid
  AND deleted_at IS NULL
ORDER BY id
LIMIT sqlc.arg('row_limit');

-- SetTaskTitleUnlessTaken changes a live task's title from old_title to title, unless it no longer
-- has old_title (changed since it was read, by the apps that keep running) or another live task of
-- the same owner (or, for an ownerless task, another ownerless one) has title; then it changes
-- nothing (0 rows). Two checks, not one with IS NOT DISTINCT FROM, which no index serves: each is a
-- lookup in tasks_title_key instead of a scan of the table for every task changed.
-- name: SetTaskTitleUnlessTaken :execrows
UPDATE tasks t
SET title = sqlc.arg('title'),
    updated_at = now()
WHERE t.id = sqlc.arg('id')
  AND t.title = sqlc.arg('old_title')
  AND t.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM tasks other
    WHERE other.owner_id = t.owner_id
      AND other.title = sqlc.arg('title')
      AND other.id <> t.id
      AND other.deleted_at IS NULL
  )
  AND NOT (
    t.owner_id IS NULL
    AND EXISTS (
      SELECT 1
      FROM tasks other
      WHERE other.owner_id IS NULL
        AND other.title = sqlc.arg('title')
        AND other.id <> t.id
        AND other.deleted_at IS NULL
    )
  );

-- TaskHasTitle says whether a task is live and still titled title.
-- name: TaskHasTitle :one
SELECT EXISTS (
  SELECT 1
  FROM tasks
  WHERE id = sqlc.arg('id')
    AND title = sqlc.arg('title')
    AND deleted_at IS NULL
);
