-- The schema as it should be now. Change it here, then run `make migrate-diff name=<what changed>`
-- to have Atlas write the migration, and `make generate` to update the sqlc code.

-- The people the app serves. The sign-in provider proves who someone is (auth_subject, its ID
-- for them); what they may do is decided on our data, so changing provider keeps every user's rows
-- and rights. Other tables point at id, never at auth_subject. auth_subject is unique only within
-- one provider: to trust two at once (say, while moving), make it unique on (issuer, subject), or
-- someone at one could sign in as a namesake at the other.
CREATE TABLE users (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  auth_subject text NOT NULL,
  -- Admins see and change everyone's rows (tasks: not only their own). Set by hand, after their
  -- first sign-in:
  -- UPDATE users SET is_admin = true WHERE auth_subject = '<their ID at the sign-in provider>';
  is_admin boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_pkey PRIMARY KEY (id),
  CONSTRAINT users_auth_subject_key UNIQUE (auth_subject),
  CONSTRAINT users_auth_subject_length CHECK (char_length(auth_subject) BETWEEN 1 AND 255)
);

CREATE TABLE tasks (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  title text NOT NULL,
  done boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- Set when the task is deleted; NULL while it's live. See docs/standards/database.md.
  deleted_at timestamptz,
  -- Who made it, and the only one besides admins who sees it. NULL for tasks made before tasks had
  -- owners, including those the old app made during that deploy: only admins see those.
  owner_id uuid,
  CONSTRAINT tasks_pkey PRIMARY KEY (id),
  CONSTRAINT tasks_owner_id_fkey FOREIGN KEY (owner_id) REFERENCES users (id),
  CONSTRAINT tasks_title_length CHECK (char_length(title) BETWEEN 1 AND 200)
);

-- Titles are unique among one owner's live tasks: another user's titles neither block nor show
-- through a duplicate error, and a deleted task's title can be used again. The service matches a
-- duplicate by this name.
CREATE UNIQUE INDEX tasks_title_key ON tasks (owner_id, title) NULLS NOT DISTINCT
  WHERE deleted_at IS NULL;

-- Serves a user's own list: newest first, paged by (created_at, id), live tasks only.
CREATE INDEX tasks_owner_id_created_at_id_idx ON tasks (owner_id, created_at DESC, id DESC)
  WHERE deleted_at IS NULL;

-- Serves an admin's list of everyone's tasks.
CREATE INDEX tasks_created_at_id_idx ON tasks (created_at DESC, id DESC) WHERE deleted_at IS NULL;

-- Jobs waiting to be queued. A job is written here in the same transaction as the rows it's about,
-- so both are saved or neither is; the worker moves each to the job queue, under its id, and
-- deletes it. Rows here are seconds old: older ones mean the queue can't take them. A project
-- without a worker doesn't use it: drop it with a migration of its own.
CREATE TABLE outbox (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  job_type text NOT NULL,
  queue text NOT NULL,
  payload bytea NOT NULL,
  timeout_seconds integer NOT NULL,
  max_retries integer NOT NULL,
  -- The API request that queued it, for the job's log lines; NULL when no request did.
  request_id text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT outbox_pkey PRIMARY KEY (id)
);

-- Serves taking the oldest jobs first.
CREATE INDEX outbox_created_at_idx ON outbox (created_at);
