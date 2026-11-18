package startup

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestCheckProfileAddr(t *testing.T) {
	t.Parallel()
	for addr, ok := range map[string]bool{
		"":               true,
		"127.0.0.1:6060": true,
		"[::1]:6060":     true,
		"localhost:6060": true,
		"127.0.0.2:6060": true,  // all of 127.0.0.0/8 is this machine
		":6060":          false, // every interface
		"0.0.0.0:6060":   false,
		"[::]:6060":      false,
		"10.0.0.5:6060":  false,
		"example.com:80": false,
		"6060":           false,
		"127.0.0.1":      false,
	} {
		if err := CheckProfileAddr(addr); (err == nil) != ok {
			t.Errorf("CheckProfileAddr(%q) = %v, want ok = %v", addr, err, ok)
		}
	}
}

// testClient has a time limit, unlike Go's default; a 1 s CPU profile is the slowest call.
var testClient = &http.Client{Timeout: 10 * time.Second}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestProfilePages(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(profileHandler())
	t.Cleanup(server.Close)

	// Go takes one CPU profile and one trace at a time: not while go test takes its own.
	profiling := flag.Lookup("test.cpuprofile").Value.String() != "" || flag.Lookup("test.trace").Value.String() != ""
	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
		takesCPU   bool
	}{
		{"/debug/pprof/", 200, "goroutine", false},
		{"/debug/pprof/heap?debug=1", 200, "heap profile", false},
		{"/debug/pprof/goroutine?debug=1", 200, "goroutine profile", false},
		{"/debug/pprof/profile?seconds=1", 200, "", true},
		{"/debug/pprof/cmdline", 404, "", false}, // the program's arguments
		{"/debug/pprof/trace?seconds=0.5", 200, "", true},
		{"/debug/pprof/profile?seconds=0.5", 400, "a whole number", false}, // profile would run 30 s
		{"/debug/pprof/profile?seconds=61", 400, "seconds must be a whole number from 1 to 60", false},
		{"/debug/pprof/profile?seconds=0", 400, "", false},
		{"/debug/pprof/profile?seconds=NaN", 400, "", false},
		{"/debug/pprof/trace?seconds=600", 400, "", false},
		{"/debug/pprof/heap?seconds=3600", 400, "", false}, // a difference over that time
		{"/debug/pprof/block?seconds=3600", 400, "", false},
	}
	for _, tt := range tests {
		if tt.takesCPU && profiling {
			continue
		}
		status, body := get(t, server.URL+tt.path)
		if status != tt.wantStatus || !strings.Contains(body, tt.wantBody) {
			t.Errorf("GET %s = %d, want %d holding %q", tt.path, status, tt.wantStatus, tt.wantBody)
		}
	}
}

func TestServeProfiles(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	stop, err := ServeProfiles(t.Context(), "127.0.0.1:0", zerolog.New(&logs))
	if err != nil {
		t.Fatalf("ServeProfiles() error = %v", err)
	}
	var line struct {
		Level   string `json:"level"`
		Addr    string `json:"addr"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Level != "warn" || line.Message != "profiling on" {
		t.Fatalf("log = %q, want one warn line, profiling on", logs.String())
	}

	if status, _ := get(t, "http://"+line.Addr+"/debug/pprof/"); status != 200 {
		t.Errorf("GET /debug/pprof/ = %d, want 200", status)
	}
	stop()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+line.Addr+"/debug/pprof/", nil)
	if resp, err := testClient.Do(req); err == nil {
		resp.Body.Close()
		t.Error("profiles still served after stop")
	}
}

func TestServeProfilesOff(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	stop, err := ServeProfiles(t.Context(), "", zerolog.New(&logs))
	if err != nil || logs.Len() != 0 {
		t.Fatalf("ServeProfiles(\"\") = %v, logged %q; want nothing", err, logs.String())
	}
	stop()
}

// pprof also reads seconds from a form body, and does nothing useful with other methods.
func TestProfilePagesRefuseOtherRequests(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(profileHandler())
	t.Cleanup(server.Close)

	for _, tt := range []struct {
		method, path, body string
		wantStatus         int
	}{
		{http.MethodPost, "/debug/pprof/profile?seconds=1", "seconds=3600", 405},
		{http.MethodPut, "/debug/pprof/heap", "", 405},
		{http.MethodPost, "/debug/pprof/symbol", "0x0", 200},
		{http.MethodPost, "/debug/pprof/symbol", strings.Repeat("0x1+", maxSymbolBody/4+1), 413}, // pprof keeps it all in memory
	} {
		req, err := http.NewRequestWithContext(t.Context(), tt.method, server.URL+tt.path, strings.NewReader(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tt.method, tt.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.wantStatus {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.wantStatus)
		}
	}
}

// ServeProfiles checks the address itself, not only the settings that call it.
func TestServeProfilesRefusesOtherAddresses(t *testing.T) {
	t.Parallel()
	if stop, err := ServeProfiles(t.Context(), "0.0.0.0:0", zerolog.Nop()); err == nil {
		stop()
		t.Error("ServeProfiles(0.0.0.0:0) error = nil, want it refused")
	}
}
