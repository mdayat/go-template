-- Create "tasks" table
CREATE TABLE "tasks" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "title" text NOT NULL,
  "done" boolean NOT NULL DEFAULT false,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "tasks_title_key" UNIQUE ("title"),
  CONSTRAINT "tasks_title_length" CHECK ((char_length(title) >= 1) AND (char_length(title) <= 200))
);
-- Create index "tasks_created_at_id_idx" to table: "tasks"
CREATE INDEX "tasks_created_at_id_idx" ON "tasks" ("created_at" DESC, "id" DESC);
