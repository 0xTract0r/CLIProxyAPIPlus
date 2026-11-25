package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ce "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCodexConfigRequiresExactKeyAndOrigin(t *testing.T) {
	cfg := &config.Config{CodexKey: []config.CodexKey{
		{APIKey: "Fixture", BaseURL: "https://one.invalid/v1"},
		{APIKey: "fixture", BaseURL: "https://one.invalid/v1"},
		{APIKey: "Fixture", BaseURL: "https://two.invalid/v1"},
	}}
	cfg.CodexKey[0].Models = []config.CodexModel{{Name: "fixture-model", IsCompat: true}}
	for _, test := range []struct {
		name, key, base, index string
		want                   int
	}{
		{"exact", "Fixture", "https://one.invalid/v1", "", 0},
		{"case-sensitive-key", "fixture", "https://one.invalid:443/v1/", "0", 1},
		{"wrong-case-key", "FIXTURE", "https://one.invalid/v1", "", -1},
		{"different-host", "Fixture", "https://three.invalid/v1", "", -1},
		{"different-port", "Fixture", "https://one.invalid:444/v1", "0", -1},
		{"different-path", "Fixture", "https://one.invalid/V1", "0", -1},
		{"stale-index", "Fixture", "https://two.invalid/v1", "0", 2},
		{"base-alone", "", "https://one.invalid/v1", "0", -1},
		{"custom-config-is-not-default-origin", "Fixture", "", "0", -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cred := &auth.Auth{Attributes: map[string]string{"api_key": test.key, "base_url": test.base, auth.AttributeConfigIndex: test.index}}
			got := resolveCodexKeyConfig(cfg, cred)
			if test.want < 0 && got != nil || test.want >= 0 && got != &cfg.CodexKey[test.want] {
				t.Fatal("wrong credential config selected")
			}
			cred.Provider = "codex"
			executor := NewCodexExecutor(cfg)
			if executor.resolveCodexModelIsCompat(cred, ce.Request{Model: "fixture-model"}, "fixture-model") != (test.want == 0) {
				t.Fatal("wrong credential config affected model compatibility")
			}
		})
	}
}

type ordinaryCommonRT func(*http.Request) (*http.Response, error)

func (r ordinaryCommonRT) RoundTrip(req *http.Request) (*http.Response, error) { return r(req) }

func TestOrdinaryCodexHTTPPreservesExplicitTierAndRequestID(t *testing.T) {
	for _, fixture := range []struct{ name, id, wantID, requestTier string }{
		{"explicit", "request-01", "request-01", `,"service_tier":"priority"`},
		{"untrusted-request-id", "has space", "", ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", ordinaryCommonRT(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "origin.invalid" || req.Header.Get("Authorization") != "Bearer fixture" || req.Header.Get("X-Client-Request-Id") != fixture.wantID {
					t.Fatalf("wrong origin, credential or request ID")
				}
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				tier := ojson.GetBytesNoCopy(raw, "service_tier")
				if fixture.requestTier == "" && tier.Exists() || fixture.requestTier != "" && tier.String() != "priority" {
					t.Fatalf("unexpected tier %s", tier.Raw)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			}))
			cred := &auth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://origin.invalid"}}
			payload := []byte(`{"model":"gpt-5","instructions":"","input":[]` + fixture.requestTier + "}")
			exec := NewCodexExecutor(&config.Config{})
			resp, _, err := exec.sendOrdinaryCodex(ctx, cred, ce.Request{Model: "gpt-5", Payload: payload}, ce.Options{SourceFormat: tr.FormatCodex, Headers: http.Header{"X-Client-Request-Id": {fixture.id}}}, "/responses", true, false, nil)
			if err != nil || calls != 1 || resp == nil {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			resp.Body.Close()
		})
	}
}
