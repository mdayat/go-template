-- The schema as it should be now. Change it here, then run `make migrate-diff name=<what changed>`
-- to have Atlas write the migration, and `make generate` to update the sqlc code.

CREATE TABLE tasks (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  title text NOT NULL,
  done boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tasks_pkey PRIMARY KEY (id),
  CONSTRAINT tasks_title_key UNIQUE (title),
  CONSTRAINT tasks_title_length CHECK (char_length(title) BETWEEN 1 AND 200)
);

-- Serves the list: newest first, paged by (created_at, id).
CREATE INDEX tasks_created_at_id_idx ON tasks (created_at DESC, id DESC);
