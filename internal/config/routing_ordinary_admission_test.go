package config

import (
	"strings"
	"testing"
)

func TestOrdinaryCredentialAdmissionConfig(t *testing.T) {
	for _, c := range []struct {
		name, yaml string
		want       int
		valid      bool
	}{
		{"legacy", "port: 8317\nrouting: {max-concurrent-per-credential: 2}", 2, true},
		{"v8", "server: {port: 8317}\nrouting: {max-concurrent-per-credential: 3, session-affinity: true, session-affinity-rebind-on: never}", 3, true},
		{"default", "port: 8317", 0, true},
		{"negative", "port: 8317\nrouting: {max-concurrent-per-credential: -1}", -1, false},
		{"adaptive", "port: 8317\nrouting: {strategy: adaptive}", 0, false},
		{"quota rebinding", "port: 8317\nrouting: {session-affinity: true, session-affinity-rebind-on: quota}", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(c.yaml))
			if c.name == "adaptive" {
				if err == nil || !strings.Contains(err.Error(), "adaptive is unsupported") {
					t.Fatalf("legacy adaptive should be rejected during parsing: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if limit := cfg.Routing.MaxConcurrentPerCredential; limit != c.want {
				t.Fatalf("limit=%d want=%d", limit, c.want)
			}
			if err := cfg.ValidateOrdinaryRouting(); (err == nil) != c.valid {
				t.Fatalf("validation error: %v", err)
			}
		})
	}
}
