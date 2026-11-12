// Command go-template starts the application.
package main

import (
	"path/filepath"
	"strconv"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/mdayat/go-template/configs/env"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	zerolog.CallerMarshalFunc = func(_ uintptr, file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}

	logger := log.With().Caller().Logger()
	if _, err := env.Load(); err != nil {
		logger.Fatal().Err(err).Send()
	}
}
