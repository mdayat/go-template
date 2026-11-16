-- Create "users" table
CREATE TABLE "users" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "auth_subject" text NOT NULL,
  "is_admin" boolean NOT NULL DEFAULT false,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "users_auth_subject_key" UNIQUE ("auth_subject"),
  CONSTRAINT "users_auth_subject_length" CHECK ((char_length(auth_subject) >= 1) AND (char_length(auth_subject) <= 255))
);
