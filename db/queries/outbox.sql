-- name: AddToOutbox :one
INSERT INTO outbox (job_type, queue, payload, timeout_seconds, max_retries, request_id)
VALUES (@job_type, @queue, @payload, @timeout_seconds, @max_retries, @request_id)
RETURNING id;

-- TakeFromOutbox takes the oldest jobs and locks them for the rest of the transaction. Rows another
-- worker has locked are skipped, not waited for, so workers take different jobs.
-- name: TakeFromOutbox :many
SELECT id, job_type, queue, payload, timeout_seconds, max_retries, request_id, created_at
FROM outbox
ORDER BY created_at
LIMIT @max_jobs
FOR UPDATE SKIP LOCKED;

-- name: DeleteFromOutbox :exec
DELETE FROM outbox WHERE id = ANY(@ids::uuid[]);

-- OutboxBacklog is how many jobs wait, and since when the oldest has (now when none does). now(),
-- not the program's clock, so the wait doesn't depend on the two clocks agreeing.
-- name: OutboxBacklog :one
SELECT count(*) AS jobs,
  extract(epoch FROM now() - coalesce(min(created_at), now()))::float8 AS oldest_wait_seconds
FROM outbox;
