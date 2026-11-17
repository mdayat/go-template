package startup

import (
	"bytes"
	"errors"
	"math"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/rs/zerolog"
)

// Go's memory limit and the environment belong to the whole test binary, so these tests don't run
// in parallel (t.Setenv refuses to) and put the limit back.
func TestLimitMemory(t *testing.T) {
	limitOf := func(n uint64, err error) memlimit.Provider {
		return func() (uint64, error) { return n, err }
	}
	notCalled := func() (uint64, error) {
		t.Error("the container's limit was read, want it skipped")
		return 0, nil
	}
	const keep = -1 // the limit stays as it was
	tests := []struct {
		name      string
		env       map[string]string
		goRead    int64 // the limit Go read from GOMEMLIMIT as the process started; 0: none
		container memlimit.Provider
		wantLimit int64
		wantLog   string
		wantErr   string
	}{
		{"90% of the container's limit", nil, 0, limitOf(1000, nil), 900, `"memory_limit_bytes":900`, ""},
		{"an empty AUTOMEMLIMIT is unset", map[string]string{"AUTOMEMLIMIT": ""}, 0, limitOf(1000, nil), 900, "memory limit set", ""},
		{"AUTOMEMLIMIT's share", map[string]string{"AUTOMEMLIMIT": "0.5"}, 0, limitOf(1000, nil), 500, `"automemlimit":"0.5"`, ""},
		{"a container with no limit", nil, 0, limitOf(0, memlimit.ErrNoLimit), math.MaxInt64, "no memory limit", ""},
		{"not in a container", nil, 0, limitOf(0, memlimit.ErrCgroupsNotSupported), math.MaxInt64, "not in a container", ""},
		{"not in a cgroup", nil, 0, limitOf(0, memlimit.ErrNoCgroup), math.MaxInt64, "not in a container", ""},
		{"the container's limit unreadable", nil, 0, limitOf(0, errors.New("open /proc/self/mountinfo: permission denied")), math.MaxInt64, `"level":"warn"`, ""},
		{"GOMEMLIMIT kept", map[string]string{"GOMEMLIMIT": "300MiB"}, 300 << 20, notCalled, keep, `"gomemlimit":"300MiB"`, ""},
		{"GOMEMLIMIT only in .env", map[string]string{"GOMEMLIMIT": "300MiB"}, 0, notCalled, keep, "", "GOMEMLIMIT"},
		{"GOMEMLIMIT off", map[string]string{"GOMEMLIMIT": "off"}, 0, notCalled, keep, "no memory limit", ""},
		{"AUTOMEMLIMIT off", map[string]string{"AUTOMEMLIMIT": "off"}, 0, notCalled, keep, `"automemlimit":"off"`, ""},
		{"AUTOMEMLIMIT not a number", map[string]string{"AUTOMEMLIMIT": "most"}, 0, notCalled, keep, "", "AUTOMEMLIMIT"},
		{"AUTOMEMLIMIT 1, all of it", map[string]string{"AUTOMEMLIMIT": "1"}, 0, limitOf(1000, nil), 1000, "memory limit set", ""},
		{"AUTOMEMLIMIT 0", map[string]string{"AUTOMEMLIMIT": "0"}, 0, notCalled, keep, "", "AUTOMEMLIMIT"},
		{"AUTOMEMLIMIT OFF, not off", map[string]string{"AUTOMEMLIMIT": "OFF"}, 0, notCalled, keep, "", "AUTOMEMLIMIT"},
		{"AUTOMEMLIMIT above 1", map[string]string{"AUTOMEMLIMIT": "1.5"}, 0, notCalled, keep, "", "AUTOMEMLIMIT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOMEMLIMIT", "")
			t.Setenv("AUTOMEMLIMIT", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			original := debug.SetMemoryLimit(-1)
			t.Cleanup(func() { debug.SetMemoryLimit(original) })
			before := int64(math.MaxInt64)
			if tt.goRead != 0 {
				before = tt.goRead
			}
			debug.SetMemoryLimit(before)
			var logs bytes.Buffer

			err := limitMemory(zerolog.New(&logs), tt.container)

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("limitMemory() error = %v, want none", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("limitMemory() error = %v, want one naming %s", err, tt.wantErr)
			}
			want := tt.wantLimit
			if want == keep {
				want = before
			}
			if got := debug.SetMemoryLimit(-1); got != want {
				t.Errorf("Go's memory limit = %d, want %d", got, want)
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log = %q, want it to hold %q", logs.String(), tt.wantLog)
			}
		})
	}
}
