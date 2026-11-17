package main

import (
	"time"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/startup"
)

// settings are what the worker reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	databaseURL string
	dbLimits    startup.DBLimits
	redisURL    string // the job queue
	// purgeWebhookURL gets a report after each purge; "" sends none.
	purgeWebhookURL string
}

// loadSettings reads the worker's settings from sources, the first winning, and reports every
// missing or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	s := settings{
		app:         r.App(),
		databaseURL: r.Required("DATABASE_URL"),
		// Statement: at least the longest job's Timeout (internal/jobs), so no job's statement is cut
		// before its job is.
		dbLimits: startup.DBLimits{
			Statement:         r.Duration("DB_STATEMENT_TIMEOUT", 5*time.Minute),
			IdleInTransaction: r.Duration("DB_IDLE_IN_TRANSACTION_TIMEOUT", time.Minute),
		},
		redisURL: r.Required("REDIS_URL"),

		purgeWebhookURL: r.OptionalHTTPURL("PURGE_WEBHOOK_URL"),
	}
	if err := r.Err(); err != nil {
		return settings{}, err
	}
	return s, nil
}
