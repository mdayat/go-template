package main

import "github.com/mdayat/go-template/internal/config"

// settings are what the worker reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	databaseURL string
	redisURL    string // the job queue
}

// loadSettings reads the worker's settings from sources, the first winning, and reports every
// missing or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	s := settings{
		app:         r.App(),
		databaseURL: r.Required("DATABASE_URL"),
		redisURL:    r.Required("REDIS_URL"),
	}
	if err := r.Err(); err != nil {
		return settings{}, err
	}
	return s, nil
}
