package startup

import (
	"context"
	"errors"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/metrictest"
	"github.com/mdayat/go-template/internal/spantest"
	"github.com/mdayat/go-template/internal/testenv"
)

var testLimits = DBLimits{Statement: 10 * time.Second, IdleInTransaction: 10 * time.Second}

// A query's time is recorded under its sqlc name, never its SQL, and the pool's use is recorded too.
func TestConnectDBRecordsMetrics(t *testing.T) {
	t.Parallel()
	url, err := testenv.Get("TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	rec := metrictest.New()
	pool, err := ConnectDB(t.Context(), url, testLimits, rec.Provider, tracenoop.NewTracerProvider())
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

// A query in a request or job is a span named by its sqlc name ("query Echo"), with its SQL but
// never its values.
func TestConnectDBTraces(t *testing.T) {
	t.Parallel()
	url, err := testenv.Get("TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	spans := spantest.New()
	pool, err := ConnectDB(t.Context(), url, testLimits, noop.NewMeterProvider(), spans.Provider)
	if err != nil {
		t.Fatalf("ConnectDB() error = %v", err)
	}
	t.Cleanup(pool.Close)
	ctx, parent := spans.Provider.Tracer("test").Start(t.Context(), "request")

	var got string
	if err := pool.QueryRow(ctx, "-- name: Echo :one\nSELECT $1::text", "personal-data").Scan(&got); err != nil {
		t.Fatal(err)
	}
	parent.End()

	echo := spans.Named("query Echo")
	if len(echo) != 1 || echo[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("spans = %q, want one query Echo under the request", spans.Names())
	}
	attrs := spantest.Attributes(echo[0])
	if !strings.Contains(attrs["db.query.text"], "SELECT $1::text") {
		t.Errorf("db.query.text = %q, want the SQL", attrs["db.query.text"])
	}
	for k, v := range attrs {
		if strings.Contains(v, "personal-data") {
			t.Errorf("attribute %s = %q holds a query's value", k, v)
		}
	}
}

// A query whose context ends stops on the database too, not only in the app: otherwise it runs on,
// holding its locks, long after the request that asked for it has answered. pgx sends Postgres a
// cancel request when it gives up on a query; this fails if a pgx change or setting stops that.
func TestConnectDBStopsCancelledQueries(t *testing.T) {
	t.Parallel()
	url, err := testenv.Get("TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := ConnectDB(t.Context(), url, testLimits, noop.NewMeterProvider(), tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("ConnectDB() error = %v", err)
	}
	t.Cleanup(pool.Close)
	marker := "cancel-" + uuid.NewString()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := pool.Exec(ctx, "SELECT pg_sleep(5) -- "+marker); err == nil {
		t.Fatal("Exec() error = nil, want the context's")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var running int
		err := pool.QueryRow(t.Context(),
			"SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query LIKE '%' || $1 || '%' AND pid <> pg_backend_pid()",
			marker).Scan(&running)
		if err != nil {
			t.Fatal(err)
		}
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled query still runs on the database 2 s later")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Every connection has the limits, and Postgres enforces them.
func TestConnectDBSetsTimeLimits(t *testing.T) {
	t.Parallel()
	url, err := testenv.Get("TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	limits := DBLimits{Statement: 300 * time.Millisecond, IdleInTransaction: 250 * time.Millisecond}
	pool, err := ConnectDB(t.Context(), url, limits, noop.NewMeterProvider(), tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("ConnectDB() error = %v", err)
	}
	t.Cleanup(pool.Close)

	var statement, idle string
	err = pool.QueryRow(t.Context(),
		"SELECT current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout')").
		Scan(&statement, &idle)
	if err != nil {
		t.Fatal(err)
	}
	if statement != "300ms" || idle != "250ms" {
		t.Errorf("statement_timeout = %s, idle_in_transaction_session_timeout = %s; want 300ms, 250ms", statement, idle)
	}

	_, err = pool.Exec(t.Context(), "SELECT pg_sleep(2)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("a statement past the limit: error = %v, want Postgres's 57014 (query_canceled)", err)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	time.Sleep(1500 * time.Millisecond) // well past the limit, so a busy machine can't make it flaky
	if _, err := tx.Exec(t.Context(), "SELECT 1"); err == nil {
		t.Error("a transaction left open past the limit still runs queries, want its connection ended")
	}
}

// A limit set in the URL too would leave one of the two values silently ignored; 0 would be none,
// and Postgres refuses one too long.
func TestConnectDBRefusesOtherTimeLimits(t *testing.T) {
	t.Parallel()
	url, err := testenv.Get("TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	with := func(key, value string) string {
		u, err := neturl.Parse(url)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set(key, value)
		u.RawQuery = q.Encode()
		return u.String()
	}
	tests := []struct {
		name    string
		url     string
		limits  DBLimits
		setting string
	}{
		{"statement_timeout in the URL", with("statement_timeout", "1000"), testLimits, "DB_STATEMENT_TIMEOUT"},
		{"idle timeout in the URL", with("idle_in_transaction_session_timeout", "1000"), testLimits, "DB_IDLE_IN_TRANSACTION_TIMEOUT"},
		{"statement_timeout in another case", with("Statement_Timeout", "1000"), testLimits, "DB_STATEMENT_TIMEOUT"},
		{"statement_timeout in options", with("options", "-c statement_timeout=1000"), testLimits, "DB_STATEMENT_TIMEOUT"},
		{"statement-timeout in options", with("options", "--Statement-Timeout=1000"), testLimits, "DB_STATEMENT_TIMEOUT"},
		{"no statement limit", url, DBLimits{IdleInTransaction: time.Second}, "DB_STATEMENT_TIMEOUT"},
		{"no idle limit", url, DBLimits{Statement: time.Second}, "DB_IDLE_IN_TRANSACTION_TIMEOUT"},
		{"too long a limit", url, DBLimits{Statement: time.Second, IdleInTransaction: 600 * time.Hour}, "DB_IDLE_IN_TRANSACTION_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool, err := ConnectDB(t.Context(), tt.url, tt.limits, noop.NewMeterProvider(), tracenoop.NewTracerProvider())
			if err == nil {
				pool.Close()
				t.Fatal("ConnectDB() error = nil, want one")
			}
			if !strings.Contains(err.Error(), tt.setting) {
				t.Errorf("ConnectDB() error = %q, want it to name %s", err, tt.setting)
			}
			if strings.Contains(err.Error(), "postgres:postgres") {
				t.Errorf("ConnectDB() error = %q quotes the URL's password", err)
			}
		})
	}
}
