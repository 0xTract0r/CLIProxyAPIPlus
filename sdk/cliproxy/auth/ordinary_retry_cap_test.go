package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryRetryCredentialCapLegacyValues(t *testing.T) {
	m := NewManager(nil, nil, nil)
	for _, tc := range []struct{ input, want int }{
		{0, internalconfig.DefaultMaxRetryCredentials},
		{3, 3},
		{-1, internalconfig.DefaultMaxRetryCredentials},
	} {
		m.SetRetryConfig(1, time.Second, tc.input)
		if got := int(m.maxRetryCredentials.Load()); got != tc.want {
			t.Errorf("credential limit with %d = %d, want %d", tc.input, got, tc.want)
		}
	}
}
