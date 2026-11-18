-- Create "outbox" table
CREATE TABLE "outbox" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "job_type" text NOT NULL,
  "queue" text NOT NULL,
  "payload" bytea NOT NULL,
  "timeout_seconds" integer NOT NULL,
  "max_retries" integer NOT NULL,
  "request_id" text NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "outbox_created_at_idx" to table: "outbox"
CREATE INDEX "outbox_created_at_idx" ON "outbox" ("created_at");
