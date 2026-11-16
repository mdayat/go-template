-- Drop index "tasks_created_at_id_idx" from table: "tasks"
DROP INDEX "tasks_created_at_id_idx";
-- Modify "tasks" table
ALTER TABLE "tasks" DROP CONSTRAINT "tasks_title_key", ADD COLUMN "deleted_at" timestamptz NULL;
-- Create index "tasks_created_at_id_idx" to table: "tasks"
CREATE INDEX "tasks_created_at_id_idx" ON "tasks" ("created_at" DESC, "id" DESC) WHERE (deleted_at IS NULL);
-- Create index "tasks_title_key" to table: "tasks"
CREATE UNIQUE INDEX "tasks_title_key" ON "tasks" ("title") WHERE (deleted_at IS NULL);
