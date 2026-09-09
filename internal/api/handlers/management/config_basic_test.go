package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNormalizeRoutingStrategy locks the management-side input validation for
// PUT /v0/management/routing/strategy. In particular it pins the fork addition
// that accepts the "adaptive" account-scheduling strategy so it can be
// hot-switched via the management API (previously only round-robin / fill-first
// were accepted, forcing a config edit + container restart to enable adaptive).
func TestNormalizeRoutingStrategy(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"empty defaults to round-robin", "", "round-robin", true},
		{"round-robin canonical", "round-robin", "round-robin", true},
		{"roundrobin alias", "roundrobin", "round-robin", true},
		{"rr alias", "rr", "round-robin", true},
		{"RR uppercase", "RR", "round-robin", true},
		{"round-robin padded", "  round-robin  ", "round-robin", true},
		{"fill-first canonical", "fill-first", "fill-first", true},
		{"fillfirst alias", "fillfirst", "fill-first", true},
		{"ff alias", "ff", "fill-first", true},
		{"adaptive canonical", "adaptive", config.RoutingStrategyAdaptive, true},
		{"ADAPTIVE uppercase", "ADAPTIVE", config.RoutingStrategyAdaptive, true},
		{"Adaptive padded", "  Adaptive ", config.RoutingStrategyAdaptive, true},
		{"bogus rejected", "bogus", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeRoutingStrategy(tc.input)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("normalizeRoutingStrategy(%q) = (%q, %v), want (%q, %v)",
					tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}
