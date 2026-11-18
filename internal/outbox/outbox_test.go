package outbox

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/json"
	"github.com/mdayat/go-template/internal/metrictest"
	"github.com/mdayat/go-template/internal/postgres"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/testqueue"
	"github.com/mdayat/go-template/internal/testredis"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// testJob is a job for queueName.
func testJob(queueName, payload string) queue.Job {
	return queue.Job{Type: "test:job", Queue: queueName, Payload: []byte(payload), Timeout: time.Minute, MaxRetries: 2}
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) []db.Outbox {
	t.Helper()
	rows, err := db.New(pool).TakeFromOutbox(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// outboxCount counts the outbox's rows without locking them (outboxRows does, and a relay running
// meanwhile would skip those).
func outboxCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func userCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requestContext is a context as the API's request log leaves it, and that request's ID.
func requestContext(t *testing.T) (context.Context, string) {
	t.Helper()
	var ctx context.Context
	var id string
	hlog.RequestIDHandler("request_id", "X-Request-Id")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
		rid, _ := hlog.IDFromRequest(r)
		id = rid.String()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	return ctx, id
}

// A job added in a transaction is saved with its rows, or not at all.
func TestAddInATransaction(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	ctx, requestID := requestContext(t)

	failed := errors.New("a later step failed")
	err := postgres.InTx(ctx, pool, func(q *db.Queries) error {
		if _, err := q.CreateUser(ctx, "someone"); err != nil {
			return err
		}
		if _, err := Add(ctx, q, testJob("default", `{"n":1}`)); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) || userCount(t, pool) != 0 || len(outboxRows(t, pool)) != 0 {
		t.Fatalf("failed transaction: err %v, %d users, %d jobs; want its error, 0, 0", err, userCount(t, pool), len(outboxRows(t, pool)))
	}

	var id string
	err = postgres.InTx(ctx, pool, func(q *db.Queries) error {
		if _, err := q.CreateUser(ctx, "someone"); err != nil {
			return err
		}
		id, err = Add(ctx, q, testJob("default", `{"n":1}`))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, pool)
	if userCount(t, pool) != 1 || len(rows) != 1 {
		t.Fatalf("saved transaction: %d users, %d jobs; want 1, 1", userCount(t, pool), len(rows))
	}
	got := jobFrom(rows[0])
	want := testJob("default", `{"n":1}`)
	want.ID, want.RequestID = id, requestID
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("kept job (-want +got):\n%s", diff)
	}
}

// What a queue would refuse later is refused now: a job stuck in the outbox would block nothing,
// but would never run either.
func TestAddRefuses(t *testing.T) {
	t.Parallel()
	q := db.New(testdb.New(t))
	withID := testJob("default", "{}")
	withID.ID = "mine"
	for name, job := range map[string]queue.Job{
		"no queue":       testJob("", "{}"),
		"its own ID":     withID,
		"a part-second":  {Type: "test:job", Queue: "default", Timeout: 1500 * time.Millisecond},
		"years of time":  {Type: "test:job", Queue: "default", Timeout: 100 * 365 * 24 * time.Hour},
		"no type at all": {Queue: "default", Timeout: time.Second},
		"a huge payload": {Type: "test:job", Queue: "default", Timeout: time.Second, Payload: make([]byte, MaxPayload+1)},
	} {
		if _, err := Add(t.Context(), q, job); err == nil {
			t.Errorf("Add(%s) = nil error, want one", name)
		}
	}
}

// The relay queues each job under its outbox row's ID, with its request ID, and deletes it; a job
// the queue holds already (the relay queued it, then failed to delete it) is done, not run twice.
func TestRelayMovesJobs(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	client := testredis.New(t)
	queueName := testqueue.NewQueue(t, client)
	enqueuer := valkey.NewEnqueuer(client, tracenoop.NewTracerProvider())
	relay := NewRelay(pool, enqueuer, zerolog.Nop())
	ctx, requestID := requestContext(t)
	q := db.New(pool)

	first, err := Add(ctx, q, testJob(queueName, `{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Add(t.Context(), q, testJob(queueName, `{"n":2}`))
	// As if a relay had queued it and then failed to delete its row.
	queuedAlready := testJob(queueName, `{"n":2}`)
	queuedAlready.ID = second
	if _, err := enqueuer.Enqueue(t.Context(), queuedAlready); err != nil {
		t.Fatal(err)
	}

	moved, err := relay.moveOnce(t.Context())
	if err != nil || moved != 2 || len(outboxRows(t, pool)) != 0 {
		t.Fatalf("moveOnce = %d, %v with %d jobs left; want 2, nil, 0", moved, err, len(outboxRows(t, pool)))
	}
	info := testqueue.Take(t, client, queueName, first)
	if info.Type != "test:job" || string(info.Payload) != `{"n":1}` || info.MaxRetry != 2 || info.Timeout != time.Minute ||
		info.Headers["x-request-id"] != requestID {
		t.Errorf("queued job = %+v, want test:job {\"n\":1}, 2 retries, 1m, request %s", info, requestID)
	}
	testqueue.Take(t, client, queueName, second) // queued once, by the test
}

// While the queue is down jobs wait in the outbox, and go once it's back.
func TestRelayQueueDown(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1}) // nothing listens
	t.Cleanup(func() { _ = down.Close() })
	client := testredis.New(t)
	queueName := testqueue.NewQueue(t, client)
	var logs bytes.Buffer
	relay := NewRelay(pool, valkey.NewEnqueuer(down, tracenoop.NewTracerProvider()), zerolog.New(&logs))
	id, _ := Add(t.Context(), db.New(pool), testJob(queueName, "{}"))

	for range 2 {
		moved, err := relay.moveOnce(t.Context())
		relay.report(err)
		if err == nil || moved != 0 || len(outboxRows(t, pool)) != 1 {
			t.Fatalf("moveOnce with the queue down = %d, %v with %d jobs left; want 0, an error, 1", moved, err, len(outboxRows(t, pool)))
		}
	}
	relay.queue = valkey.NewEnqueuer(client, tracenoop.NewTracerProvider())
	moved, err := relay.moveOnce(t.Context())
	relay.report(err)
	if err != nil || moved != 1 {
		t.Fatalf("moveOnce once the queue is back = %d, %v; want 1, nil", moved, err)
	}
	testqueue.Take(t, client, queueName, id)

	var messages []string
	for line := range bytes.SplitSeq(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var l struct{ Message string }
		_ = json.Unmarshal(line, &l)
		messages = append(messages, l.Message)
	}
	want := []string{"outbox: jobs aren't moving to the queue; they wait in the outbox", "outbox: jobs moving to the queue again"}
	if !cmp.Equal(messages, want) {
		t.Errorf("relay log = %q, want the change each way, once", messages)
	}
}

// recorder is a queue that keeps what it's given, to count how often each job was queued.
type recorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *recorder) Enqueue(_ context.Context, job queue.Job) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, job.ID)
	return job.ID, nil
}

// failAfter is a queue that takes n jobs, then fails as a queue that went down would.
type failAfter struct {
	recorder
	n int
}

func (f *failAfter) Enqueue(ctx context.Context, job queue.Job) (string, error) {
	if len(f.ids) >= f.n {
		return "", errors.New("connection refused")
	}
	return f.recorder.Enqueue(ctx, job)
}

// A queue that fails partway keeps what it took: those jobs are deleted from the outbox, and the
// rest wait for the next try.
func TestRelayKeepsWhatTheQueueTook(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	q := db.New(pool)
	for range 3 {
		if _, err := Add(t.Context(), q, testJob("default", "{}")); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := NewRelay(pool, &failAfter{n: 2}, zerolog.Nop()).moveOnce(t.Context())
	if err == nil || moved != 2 || len(outboxRows(t, pool)) != 1 {
		t.Errorf("moveOnce with the queue failing on the third = %d, %v with %d left; want 2, an error, 1", moved, err, len(outboxRows(t, pool)))
	}
}

// Relays in several worker instances take different jobs: each is queued once.
func TestRelaysShareTheOutbox(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	q := db.New(pool)
	var want []string
	for range 2*relayBatch + 50 {
		id, err := Add(t.Context(), q, testJob("default", "{}"))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	rec := &recorder{}
	var wg sync.WaitGroup
	for range 3 {
		relay := NewRelay(pool, rec, zerolog.Nop())
		wg.Go(func() {
			for {
				if moved, err := relay.moveOnce(t.Context()); err != nil || moved == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	slices.Sort(want)
	slices.Sort(rec.ids)
	if !cmp.Equal(want, rec.ids) {
		t.Errorf("queued %d jobs (%d distinct), want each of the %d once", len(rec.ids), len(slices.Compact(slices.Clone(rec.ids))), len(want))
	}
}

// Run keeps moving jobs until its context ends.
func TestRelayRun(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	rec := &recorder{}
	relay := NewRelay(pool, rec, zerolog.Nop())
	relay.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { relay.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond) // past its first look, at an empty outbox

	id, _ := Add(t.Context(), db.New(pool), testJob("default", "{}"))
	deadline := time.Now().Add(5 * time.Second)
	for outboxCount(t, pool) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !cmp.Equal(rec.ids, []string{id}) {
		t.Errorf("Run queued %q, want [%s]", rec.ids, id)
	}
}

// The backlog shows jobs waiting and how long; stopped, the outbox is no longer read.
func TestRecordBacklog(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	recorder := metrictest.New()
	stop, err := recordBacklog(pool, recorder.Provider, zerolog.Nop(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := recorder.Collect(t)
	if jobs, wait := got.Find("outbox.jobs", nil), got.Find("outbox.oldest_wait", nil); len(jobs) != 1 || jobs[0].Count != 0 || len(wait) != 1 || wait[0].Count != 0 {
		t.Errorf("empty outbox: jobs %v, oldest wait %v; want 0 and 0", jobs, wait)
	}

	for range 2 {
		if _, err := Add(t.Context(), db.New(pool), testJob("default", "{}")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	got = recorder.Collect(t)
	if jobs, wait := got.Find("outbox.jobs", nil), got.Find("outbox.oldest_wait", nil); len(jobs) != 1 || jobs[0].Count != 2 || len(wait) != 1 || wait[0].Count < 1 {
		t.Errorf("two waiting over a second: jobs %v, oldest wait %v; want 2 and at least 1", jobs, wait)
	}

	stop()
	if got := recorder.Collect(t); len(got["outbox.jobs"]) != 0 {
		t.Errorf("after stop: outbox.jobs = %v, want nothing", got["outbox.jobs"])
	}
}

// A backlog that can't be read is left out, logged once, not on every send.
func TestRecordBacklogLogsOnce(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	recorder := metrictest.New()
	var logs bytes.Buffer
	if _, err := RecordBacklog(pool, recorder.Provider, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	for range 3 {
		if got := recorder.Collect(t); len(got["outbox.jobs"]) != 0 {
			t.Errorf("unreadable outbox: outbox.jobs = %v, want nothing", got["outbox.jobs"])
		}
	}
	if n := bytes.Count(logs.Bytes(), []byte("outbox not measured")); n != 1 {
		t.Errorf("warnings over three failed reads = %d, want 1", n)
	}
}

// cancelAfter is a queue that takes jobs, and ends the relay's context once it has taken n: a
// shutdown in the middle of a batch.
type cancelAfter struct {
	recorder
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Enqueue(ctx context.Context, job queue.Job) (string, error) {
	id, err := c.recorder.Enqueue(ctx, job)
	if len(c.ids) == c.n {
		c.cancel()
	}
	return id, err
}

// A shutdown mid-batch stops taking jobs but saves the deletes of those queued: kept, they'd be
// queued again next start, and run twice if the queue had finished and dropped them meanwhile.
func TestRelayFinishesItsBatchOnShutdown(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	q := db.New(pool)
	for range 5 {
		if _, err := Add(t.Context(), q, testJob("default", "{}")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	queue := &cancelAfter{n: 2, cancel: cancel}
	moved, err := NewRelay(pool, queue, zerolog.Nop()).moveOnce(ctx)
	if err != nil || moved != 2 || len(queue.ids) != 2 || len(outboxRows(t, pool)) != 3 {
		t.Errorf("moveOnce cut off after 2 = %d, %v, %d queued, %d left; want 2, nil, 2, 3", moved, err, len(queue.ids), len(outboxRows(t, pool)))
	}
}

// A row the queue would never take is dropped, with an error logged, instead of blocking the rows
// behind it.
func TestRelayDropsWhatItCantQueue(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	_, err := pool.Exec(t.Context(), `INSERT INTO outbox (job_type, queue, payload, timeout_seconds, max_retries, created_at)
		VALUES ('test:job', 'default', '{}', 0, 0, now() - interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := Add(t.Context(), db.New(pool), testJob("default", "{}"))
	rec := &recorder{}
	var logs bytes.Buffer
	moved, err := NewRelay(pool, rec, zerolog.New(&logs)).moveOnce(t.Context())
	if err != nil || moved != 2 || !cmp.Equal(rec.ids, []string{good}) || len(outboxRows(t, pool)) != 0 {
		t.Errorf("moveOnce with a bad row first = %d, %v, queued %q, %d left; want 2, nil, [%s], 0", moved, err, rec.ids, len(outboxRows(t, pool)), good)
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"level":"error"`)) || !bytes.Contains(logs.Bytes(), []byte("job dropped")) {
		t.Errorf("log = %s, want an error saying the job was dropped", logs.String())
	}
}

// A full batch is followed at once by the next, not after the interval.
func TestRelayRunDrainsAFullOutbox(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	q := db.New(pool)
	for range relayBatch + 10 {
		if _, err := Add(t.Context(), q, testJob("default", "{}")); err != nil {
			t.Fatal(err)
		}
	}
	relay := NewRelay(pool, &recorder{}, zerolog.Nop())
	relay.interval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { relay.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for outboxCount(t, pool) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n := outboxCount(t, pool); n != 0 {
		t.Errorf("jobs left after a full batch, with an hour's interval = %d, want 0", n)
	}
}

// A job's own request ID beats the context's.
func TestAddKeepsTheJobsRequestID(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	ctx, _ := requestContext(t)
	job := testJob("default", "{}")
	job.RequestID = "mine"
	if _, err := Add(ctx, db.New(pool), job); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, pool); len(rows) != 1 || rows[0].RequestID == nil || *rows[0].RequestID != "mine" {
		t.Errorf("kept request ID = %v, want mine", rows)
	}
}

// A job with no payload is queued with an empty one: the driver would write nil as NULL, which the
// column refuses.
func TestAddWithoutAPayload(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	job := testJob("default", "")
	job.Payload = nil
	if _, err := Add(t.Context(), db.New(pool), job); err != nil {
		t.Fatalf("Add() with no payload = %v, want nil", err)
	}
	if rows := outboxRows(t, pool); len(rows) != 1 || len(rows[0].Payload) != 0 {
		t.Errorf("kept job = %v, want one with an empty payload", rows)
	}
}
