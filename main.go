package main

import (
	"path/filepath"
	"strconv"

	"github.com/mdayat/go-template/configs/env"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	zerolog.CallerMarshalFunc = func(_ uintptr, file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}

	logger := log.With().Caller().Logger()
	if err := env.Load(); err != nil {
		logger.Fatal().Err(err).Send()
	}
}
