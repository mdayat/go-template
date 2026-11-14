package main

import "github.com/mdayat/go-template/internal/config"

// settings are what the API reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	httpAddr    string
	databaseURL string
	// remove:worker
	redisURL string // the job queue
	// remove:end
}

// loadSettings reads the API's settings from sources, the first winning, and reports every missing
// or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	s := settings{
		app:         r.App(),
		httpAddr:    r.String("HTTP_ADDR", ":8080"),
		databaseURL: r.Required("DATABASE_URL"),
		// remove:worker
		redisURL: r.Required("REDIS_URL"),
		// remove:end
	}
	if err := r.Err(); err != nil {
		return settings{}, err
	}
	return s, nil
}
