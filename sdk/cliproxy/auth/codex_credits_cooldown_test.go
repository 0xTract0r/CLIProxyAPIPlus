package auth

import (
	"net/http"
	"testing"
)

func TestCodexCreditsUsable(t *testing.T) {
	cases := []struct {
		name       string
		hasCredits string
		balance    string
		unlimited  string
		want       bool
	}{
		{name: "production shape True with positive balance", hasCredits: "True", balance: "59897.4803850000", unlimited: "False", want: true},
		{name: "lowercase true with positive balance", hasCredits: "true", balance: "1", want: true},
		{name: "has credits False", hasCredits: "False", balance: "59897.48", unlimited: "False", want: false},
		{name: "zero balance not unlimited", hasCredits: "True", balance: "0", unlimited: "False", want: false},
		{name: "negative balance", hasCredits: "True", balance: "-5", want: false},
		{name: "unlimited without balance", hasCredits: "True", unlimited: "True", want: true},
		{name: "no credit headers", want: false},
		{name: "non-numeric balance", hasCredits: "True", balance: "lots", unlimited: "False", want: false},
		{name: "NaN balance", hasCredits: "True", balance: "NaN", want: false},
		{name: "has credits without balance or unlimited", hasCredits: "True", want: false},
		{name: "unlimited but has credits missing", unlimited: "True", balance: "10", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := withCodexCredits(http.Header{}, tc.hasCredits, tc.balance, tc.unlimited)
			if got := codexCreditsUsable(headers); got != tc.want {
				t.Fatalf("codexCreditsUsable() = %v, want %v (headers=%v)", got, tc.want, headers)
			}
		})
	}
	if codexCreditsUsable(nil) {
		t.Fatal("codexCreditsUsable(nil) = true, want false")
	}
}

func withCodexCredits(headers http.Header, hasCredits, balance, unlimited string) http.Header {
	if hasCredits != "" {
		headers.Set("X-Codex-Credits-Has-Credits", hasCredits)
	}
	if balance != "" {
		headers.Set("X-Codex-Credits-Balance", balance)
	}
	if unlimited != "" {
		headers.Set("X-Codex-Credits-Unlimited", unlimited)
	}
	return headers
}
