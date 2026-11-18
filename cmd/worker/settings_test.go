package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/startup"
)

// Pins the names, defaults and required settings that deploys and .example.env rely on.
func TestLoadSettings(t *testing.T) {
	t.Parallel()
	got, err := loadSettings(config.Map{
		"DATABASE_URL":      "postgres://localhost/app",
		"REDIS_URL":         "redis://localhost:6379/0",
		"PURGE_WEBHOOK_URL": "https://hooks.example.com/purged",
	})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}

	want := settings{
		app:         config.App{Env: config.EnvProduction, LogLevel: zerolog.InfoLevel},
		databaseURL: "postgres://localhost/app",
		dbLimits:    startup.DBLimits{MaxConns: 10, Statement: 5 * time.Minute, IdleInTransaction: time.Minute},
		redisURL:    "redis://localhost:6379/0",
		queues:      jobs.Queues(),

		purgeWebhookURL: "https://hooks.example.com/purged",
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(settings{})); diff != "" {
		t.Errorf("loadSettings() mismatch (-want +got):\n%s", diff)
	}

	got, err = loadSettings(config.Map{
		"DATABASE_URL":  "x",
		"REDIS_URL":     "y",
		"WORKER_QUEUES": "default=3",
	})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}
	if want := []queue.Limit{{Queue: "default", Concurrency: 3}}; !cmp.Equal(got.queues, want) || got.dbLimits.MaxConns != 3 {
		t.Errorf("WORKER_QUEUES=default=3: queues = %v, DB_MAX_CONNS = %d; want %v, 3", got.queues, got.dbLimits.MaxConns, want)
	}
}

// Fewer connections than jobs running at once would leave jobs waiting for one.
func TestLoadSettingsRefusesTooFewConnections(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{"DATABASE_URL": "x", "REDIS_URL": "y", "WORKER_QUEUES": "default=3", "DB_MAX_CONNS": "2"})
	if err == nil || !strings.Contains(err.Error(), "DB_MAX_CONNS") {
		t.Errorf("loadSettings() error = %v, want one naming DB_MAX_CONNS", err)
	}
	// A wrong WORKER_QUEUES is the only problem named, not a pool size nobody set.
	_, err = loadSettings(config.Map{"DATABASE_URL": "x", "REDIS_URL": "y", "WORKER_QUEUES": "nosuchqueue=3"})
	if err == nil || strings.Contains(err.Error(), "DB_MAX_CONNS") {
		t.Errorf("WORKER_QUEUES=nosuchqueue=3: loadSettings() error = %v, want one about WORKER_QUEUES alone", err)
	}
	// More jobs at once, over every queue, than a pool may have connections: named as WORKER_QUEUES,
	// the setting to change, even with DB_MAX_CONNS unset.
	if err := checkMaxConns(1200, 1200); err == nil || !strings.Contains(err.Error(), "WORKER_QUEUES") {
		t.Errorf("checkMaxConns(1200, 1200) = %v, want an error naming WORKER_QUEUES", err)
	}
}

func TestReadQueues(t *testing.T) {
	t.Parallel()
	known := []queue.Limit{{Queue: "default", Concurrency: 10}, {Queue: "slow", Concurrency: 2}}

	tests := []struct {
		value string
		want  []queue.Limit // nil: refused
	}{
		{"", known},
		{"default=7,slow=3", []queue.Limit{{Queue: "default", Concurrency: 7}, {Queue: "slow", Concurrency: 3}}},
		{" slow=1 ", []queue.Limit{{Queue: "slow", Concurrency: 1}}},
		{"other=1", nil},
		{"default", nil},
		{"default=0", nil},
		{"default=1001", nil},
		{"default=x", nil},
		{"default=1,default=2", nil},
		{"default=1,", nil},
	}
	for _, tt := range tests {
		got, err := readQueues(tt.value, known)
		if (err == nil) != (tt.want != nil) || !cmp.Equal(got, tt.want) {
			t.Errorf("readQueues(%q) = %v, %v; want %v", tt.value, got, err, tt.want)
		}
		if err != nil && !strings.Contains(err.Error(), "WORKER_QUEUES") {
			t.Errorf("readQueues(%q) error = %q, want it to name WORKER_QUEUES", tt.value, err)
		}
	}
}

func TestLoadSettingsNamesEveryMissingOrInvalidSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{"PURGE_WEBHOOK_URL": "hooks.example.com", "DB_STATEMENT_TIMEOUT": "0", "PPROF_ADDR": "0.0.0.0:6060"})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming every bad setting")
	}
	for _, name := range []string{"DATABASE_URL", "REDIS_URL", "PURGE_WEBHOOK_URL", "DB_STATEMENT_TIMEOUT", "PPROF_ADDR"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("loadSettings() error = %q, want it to name %s", err, name)
		}
	}
}
