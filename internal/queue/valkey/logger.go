package valkey

import (
	"fmt"

	"github.com/rs/zerolog"
)

// asynqLogger sends asynq's own messages (starting, shutting down, a lost Redis connection) to the
// JSON log, marked source=asynq. A job's result is logged by the worker's middleware, not here.
type asynqLogger struct {
	logger zerolog.Logger
}

func (l asynqLogger) Debug(args ...any) { l.log(zerolog.DebugLevel, args) }
func (l asynqLogger) Info(args ...any)  { l.log(zerolog.InfoLevel, args) }
func (l asynqLogger) Warn(args ...any)  { l.log(zerolog.WarnLevel, args) }
func (l asynqLogger) Error(args ...any) { l.log(zerolog.ErrorLevel, args) }

// Fatal exits the process, as asynq's Logger interface requires.
func (l asynqLogger) Fatal(args ...any) {
	l.logger.Fatal().Str("source", "asynq").Msg(fmt.Sprint(args...))
}

func (l asynqLogger) log(level zerolog.Level, args []any) {
	l.logger.WithLevel(level).Str("source", "asynq").Msg(fmt.Sprint(args...))
}
