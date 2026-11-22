package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoggingDisplayTimezoneFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       int
	}{
		{"default", "port: 8317\n", 8},
		{"explicit UTC", "logging-display-timezone-offset-hours: 0\n", 0},
		{"negative", "logging-display-timezone-offset-hours: -5\n", -5},
		{"out of range", "logging-display-timezone-offset-hours: 15\n", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseConfigBytes([]byte(tc.yaml))
			if err != nil || parsed.LoggingDisplayTimezoneOffsetHours != tc.want {
				t.Fatalf("parse: %v, err=%v, offset want %d", parsed, err, tc.want)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.LoggingDisplayTimezoneOffsetHours != tc.want {
				t.Fatalf("load: %v, err=%v, offset want %d", loaded, err, tc.want)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	standby, err := LoadConfigOptional(missing, true)
	if err != nil || standby.LoggingDisplayTimezoneOffsetHours != DefaultLoggingDisplayTimezoneOffsetHours {
		t.Fatalf("standby: %v, err=%v", standby, err)
	}
}
