package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryAntigravityTransportPreservesExplicitOriginAndRefusesRedirect(t *testing.T) {
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "fixture.invalid" || req.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatal("configured origin or standard selected credential changed")
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://other-fixture.invalid/redirect"}},
			Body: io.NopCloser(strings.NewReader("ordinary redirect")), Request: req}, nil
	}))
	exec := NewAntigravityExecutor(&config.Config{})
	auth := ordinaryPortAuth()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid/initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer caller-override")
	out, err := exec.HttpRequest(ctx, auth, req)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if calls != 1 || out.StatusCode != http.StatusTemporaryRedirect || req.Header.Get("Authorization") != "Bearer caller-override" {
		t.Fatal("redirect was followed or original SDK state was mutated")
	}
	mismatch, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://other-fixture.invalid/initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.HttpRequest(ctx, auth, mismatch)
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != http.StatusForbidden || calls != 1 {
		t.Fatalf("mismatched destination reached the configured transport: %v", err)
	}
}
