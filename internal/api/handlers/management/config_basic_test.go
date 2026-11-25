package management

import "testing"

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
		{"weighted-round-robin canonical", "weighted-round-robin", "weighted-round-robin", true},
		{"weighted-round-robin alias", "wrr", "weighted-round-robin", true},
		{"fill-first canonical", "fill-first", "fill-first", true},
		{"fillfirst alias", "fillfirst", "fill-first", true},
		{"ff alias", "ff", "fill-first", true},
		{"adaptive canonical", "adaptive", "", false},
		{"ADAPTIVE uppercase", "ADAPTIVE", "", false},
		{"Adaptive padded", "  Adaptive ", "", false},
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
