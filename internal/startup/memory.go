package startup

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strings"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/rs/zerolog"
)

// LimitMemory tells Go the container's memory limit, so its garbage collector works harder as
// memory nears it, instead of the program being killed for going past it. Go reads a container's
// CPU limit itself, but not its memory limit.
//
// It sets Go's limit to 90% of the container's (the rest is for memory Go doesn't manage), or to
// the share in AUTOMEMLIMIT (0.8; "off" turns this off). A GOMEMLIMIT in the environment is kept
// as it is. Call this after .env is loaded, so AUTOMEMLIMIT there counts. Where there is no
// container limit (a laptop, a container started without one), or it can't be read, Go stays
// unlimited. The start log shows the limit in force.
func LimitMemory(logger zerolog.Logger) error {
	return limitMemory(logger, memlimit.FromCgroup)
}

func limitMemory(logger zerolog.Logger, containerLimit memlimit.Provider) error {
	// Empty is unset, as for every other setting; the library would refuse it as no number.
	if v, ok := os.LookupEnv("AUTOMEMLIMIT"); ok && v == "" {
		if err := os.Unsetenv("AUTOMEMLIMIT"); err != nil {
			return fmt.Errorf("unset the empty AUTOMEMLIMIT: %w", err)
		}
	}
	// Go reads GOMEMLIMIT once, as the process starts. One loaded later from .env would make the
	// library skip its work while Go stays unlimited.
	if v := os.Getenv("GOMEMLIMIT"); v != "" && v != "off" && debug.SetMemoryLimit(-1) == math.MaxInt64 {
		return errors.New("GOMEMLIMIT is set but Go didn't read it: set it in the environment, not in .env (a size like 450MiB, or off)")
	}

	// A container limit that can't be read leaves Go unlimited, with a warning: it only tunes the
	// garbage collector, not worth refusing to start for.
	var readErr error
	read := func() (uint64, error) {
		n, err := containerLimit()
		if err != nil && !errors.Is(err, memlimit.ErrNoLimit) {
			readErr = err
			return 0, memlimit.ErrNoLimit
		}
		return n, err
	}
	limit, err := memlimit.Set(memlimit.WithProvider(read))
	if err != nil {
		return fmt.Errorf("set Go's memory limit (AUTOMEMLIMIT is a share above 0 and up to 1, or off): %w", err)
	}

	switch {
	case errors.Is(readErr, memlimit.ErrCgroupsNotSupported), errors.Is(readErr, memlimit.ErrNoCgroup):
		logger.Info().Msg("no memory limit: not in a container")
		return nil
	case readErr != nil:
		logger.Warn().Err(readErr).Msg("no memory limit: the container's can't be read")
		return nil //nolint:nilerr // warned above; the program runs unlimited, as without a container
	}
	line := logger.Info()
	for _, key := range []string{"GOMEMLIMIT", "AUTOMEMLIMIT"} {
		if v := os.Getenv(key); v != "" {
			line = line.Str(strings.ToLower(key), v)
		}
	}
	if limit == math.MaxInt64 {
		line.Msg("no memory limit")
		return nil
	}
	line.Int64("memory_limit_bytes", limit).Msg("memory limit set")
	return nil
}
