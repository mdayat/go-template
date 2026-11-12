// Atlas Community edition. db/schema.sql is the source of truth; `atlas migrate diff` compares it
// with db/migrations (replayed on a throwaway Postgres in Docker) and writes the difference as a new
// migration file.
env "local" {
  src = "file://db/schema.sql"
  url = getenv("DATABASE_URL")
  dev = "docker://postgres/17/dev?search_path=public"
  migration {
    dir = "file://db/migrations"
  }
  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}
