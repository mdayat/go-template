package main

import (
	"time"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/startup"
)

// settings are what oneoff reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	databaseURL string
	dbLimits    startup.DBLimits
}

// loadSettings reads oneoff's settings from sources, the first winning, and reports every missing
// or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	s := settings{
		app:         r.App(),
		databaseURL: r.Required("DATABASE_URL"),
		// A batch is a few hundred rows: a statement or an idle transaction this long is a stuck
		// one, holding its locks while the apps run.
		dbLimits: startup.DBLimits{
			Statement:         r.Duration("DB_STATEMENT_TIMEOUT", time.Minute),
			IdleInTransaction: r.Duration("DB_IDLE_IN_TRANSACTION_TIMEOUT", time.Minute),
		},
	}
	if err := r.Err(); err != nil {
		return settings{}, err
	}
	return s, nil
}
