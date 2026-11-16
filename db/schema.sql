-- The schema as it should be now. Change it here, then run `make migrate-diff name=<what changed>`
-- to have Atlas write the migration, and `make generate` to update the sqlc code.

CREATE TABLE tasks (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  title text NOT NULL,
  done boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- Set when the task is deleted; NULL while it's live. See docs/standards/database.md.
  deleted_at timestamptz,
  CONSTRAINT tasks_pkey PRIMARY KEY (id),
  CONSTRAINT tasks_title_length CHECK (char_length(title) BETWEEN 1 AND 200)
);

-- Titles are unique among live tasks only, so a deleted task's title can be used again. The
-- service matches a duplicate by this name.
CREATE UNIQUE INDEX tasks_title_key ON tasks (title) WHERE deleted_at IS NULL;

-- Serves the list: newest first, paged by (created_at, id), live tasks only.
CREATE INDEX tasks_created_at_id_idx ON tasks (created_at DESC, id DESC) WHERE deleted_at IS NULL;
