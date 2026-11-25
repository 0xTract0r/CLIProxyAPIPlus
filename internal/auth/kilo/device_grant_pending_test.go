package kilo

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type pendingGrantFixtureTransport func(*http.Request) (*http.Response, error)

func (f pendingGrantFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOrdinaryKiloDeviceGrantHTTP202ContinuesToApproval(t *testing.T) {
	var calls int
	client := NewKiloAuthWithHTTPClient(&http.Client{Transport: pendingGrantFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != BaseURL+"/device-auth/codes/fixture" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: http.NoBody}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"status":"approved","token":"fixture-token"}`))}, nil
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	granted, err := client.PollForToken(ctx, "fixture")
	if err != nil || calls != 2 || granted == nil || granted.Token != "fixture-token" {
		t.Fatalf("device approval after HTTP 202: calls=%d, error=%v", calls, err)
	}
}
