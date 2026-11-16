package startup

import (
	"context"
	"testing"

	"github.com/mdayat/go-template/internal/metrictest"
	"github.com/mdayat/go-template/internal/testenv"
)

// A query's time is recorded under its sqlc name, never its SQL, and the pool's use is recorded too.
func TestConnectDBRecordsMetrics(t *testing.T) {
	t.Parallel()
	url, err := testenv.Get("TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	rec := metrictest.New()
	pool, err := ConnectDB(t.Context(), url, rec.Provider)
	if err != nil {
		t.Fatalf("ConnectDB() error = %v", err)
	}
	t.Cleanup(pool.Close)

	var one int
	if err := pool.QueryRow(t.Context(), "-- name: CountOne :one\nSELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}

	got := rec.Collect(t)
	if len(got.Find("db.client.operation.duration", map[string]string{"db.operation.name": "CountOne"})) == 0 {
		t.Errorf("no query time named CountOne; recorded %v", got.Names())
	}
	pool0 := got.Find("pgxpool.acquired_connections", nil)
	if len(pool0) == 0 {
		t.Fatalf("no pool metrics; recorded %v", got.Names())
	}
	for _, p := range pool0 {
		if name := p.Attributes["db.client.connection.pool.name"]; name != "main" {
			t.Errorf("pool named %q, want main: never the database's address", name)
		}
	}
}

func TestQueryName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"-- name: GetTask :one\nSELECT * FROM tasks": "GetTask",
		"SELECT 1":            "SELECT",
		"-- name: \nSELECT 1": "SELECT",
		// An empty name falls back to otelpgx's spelling of the SQL's first word, not the next line.
		"-- name: \nselect 1": "SELECT",
	}
	for stmt, want := range tests {
		if got := queryName(context.Background(), stmt); got != want {
			t.Errorf("queryName(%q) = %q, want %q", stmt, got, want)
		}
	}
}
