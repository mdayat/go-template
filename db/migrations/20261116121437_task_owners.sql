-- Drop index "tasks_title_key" from table: "tasks"
DROP INDEX "tasks_title_key";
-- Modify "tasks" table
ALTER TABLE "tasks" ADD COLUMN "owner_id" uuid NULL, ADD
CONSTRAINT "tasks_owner_id_fkey" FOREIGN KEY ("owner_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION;
-- Create index "tasks_title_key" to table: "tasks"
CREATE UNIQUE INDEX "tasks_title_key" ON "tasks" ("owner_id", "title") NULLS NOT DISTINCT WHERE (deleted_at IS NULL);
-- Create index "tasks_owner_id_created_at_id_idx" to table: "tasks"
CREATE INDEX "tasks_owner_id_created_at_id_idx" ON "tasks" ("owner_id", "created_at" DESC, "id" DESC) WHERE (deleted_at IS NULL);
