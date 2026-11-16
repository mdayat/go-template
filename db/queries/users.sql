-- name: GetUserBySubject :one
SELECT *
FROM users
WHERE auth_subject = $1;

-- CreateUser adds someone signing in for the first time. Another request creating them at the
-- same moment wins quietly: no row comes back, and the caller reads theirs.
-- name: CreateUser :one
INSERT INTO users (auth_subject)
VALUES ($1)
ON CONFLICT (auth_subject) DO NOTHING
RETURNING *;
