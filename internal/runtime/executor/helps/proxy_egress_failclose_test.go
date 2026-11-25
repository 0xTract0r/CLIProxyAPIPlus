package helps

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestNewProxyAwareHTTPClientBlocksInvalidProxy(t *testing.T) {
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	for _, tc := range []struct {
		proxy string
		want  int
	}{
		{"invalid://fixture.invalid:1080", 0},
		{"", 1},
	} {
		client := NewProxyAwareHTTPClient(ctx, &config.Config{}, &coreauth.Auth{ID: "ordinary-proxy-egress", ProxyURL: tc.proxy}, time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if tc.want == 0 {
			if !errors.Is(err, errOutboundRoute) || resp != nil || calls != 0 {
				t.Fatalf("invalid route made %d attempts or lost its denial: %v", calls, err)
			}
		} else {
			if err != nil || resp == nil || resp.StatusCode != http.StatusNoContent || calls != 1 {
				t.Fatalf("explicit fixture transport made %d attempts: %v", calls, err)
			}
			_ = resp.Body.Close()
		}
	}
}
