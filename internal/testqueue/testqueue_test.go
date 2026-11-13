package testqueue

import "testing"

func TestLocalOptionsRefusesSharedPlaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		url    string
		wantOK bool
	}{
		{"localhost, database 1", "redis://localhost:6379/1", true},
		{"IPv4 loopback", "redis://127.0.0.1:6379/2", true},
		{"IPv6 loopback", "redis://[::1]:6379/1", true},
		{"the app's database 0", "redis://localhost:6379/0", false},
		{"no database, which is 0", "redis://localhost:6379", false},
		{"remote host", "redis://cache.example.com:6379/1", false},
		{"unix socket", "unix:///tmp/valkey.sock?db=1", false},
		{"not a redis URL", "postgres://localhost:5432/postgres", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := localOptions(tt.url)
			if gotOK := err == nil; gotOK != tt.wantOK {
				t.Errorf("localOptions(%q) error = %v, want ok = %v", tt.url, err, tt.wantOK)
			}
		})
	}
}
