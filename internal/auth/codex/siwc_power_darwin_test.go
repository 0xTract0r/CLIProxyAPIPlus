package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	ReadSIWCSystemState = func(context.Context) ([]byte, error) {
		return []byte("Current System Capabilities are: CPU Graphics Audio Network\nCurrent Power State: 4\n"), nil
	}
	os.Exit(m.Run())
}

func TestOrdinaryCodexRefreshDoesNotClaimGrantWhenPowerStateUnavailable(t *testing.T) {
	const grant = "ordinary-unavailable-power-fixture"
	journal := &FileRefreshJournal{Dir: t.TempDir()}
	previous := ReadSIWCSystemState
	t.Cleanup(func() { ReadSIWCSystemState = previous })
	errProbe := errors.New("power-state probe unavailable")
	ReadSIWCSystemState = func(context.Context) ([]byte, error) { return nil, errProbe }
	auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("token request sent without a known power state")
		return nil, nil
	})}}
	auth.SetRefreshJournal(journal)
	token, errRefresh := auth.RefreshTokens(t.Context(), grant)
	if token != nil || !errors.Is(errRefresh, errProbe) || !errors.Is(errRefresh, ErrSIWCRefreshDeferred) {
		t.Fatalf("power-state error was lost: %v", errRefresh)
	}
	raw, err := journal.RefreshRead(t.Context(), refreshGrantKey(ClientID, grant))
	if err != nil || len(raw) != 0 {
		t.Fatalf("requested refresh before readiness claimed the grant: %v", err)
	}
}

func TestOrdinaryCodexRefreshDefersDuringMaintenanceWakeThenRenews(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		probeErr error
	}{
		{name: "darkwake_missing_graphics", state: "Current System Capabilities are: CPU Network\nCurrent Power State: 2\n"},
		{name: "missing_network", state: "Current System Capabilities are: CPU Graphics Audio\n"},
		{name: "unexpected_pmset_output", state: "Graphics Network"},
		{name: "power_probe_unavailable", probeErr: errors.New("pmset unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCodexRefreshGroupForTest()
			t.Cleanup(resetCodexRefreshGroupForTest)

			previous := ReadSIWCSystemState
			t.Cleanup(func() { ReadSIWCSystemState = previous })
			currentState := tc.state
			currentErr := tc.probeErr
			checks := 0
			ReadSIWCSystemState = func(context.Context) ([]byte, error) {
				checks++
				if currentErr != nil {
					return nil, currentErr
				}
				return []byte(currentState), nil
			}

			calls := 0
			auth := NewCodexAuth(nil)
			auth.SetRefreshJournal(&FileRefreshJournal{Dir: t.TempDir()})
			auth.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"ordinary-new-access","refresh_token":"ordinary-new-refresh","expires_in":3600}`)),
					Request:    req,
				}, nil
			})}

			token, errRefresh := auth.RefreshTokensWithRetry(context.Background(), "ordinary-sleep-refresh", 3)
			if token != nil || errRefresh == nil || calls != 0 || checks != 1 {
				t.Fatalf("maintenance wake: token=%v, err=%v, calls=%d, checks=%d; want deferral without HTTP calls or retries", token, errRefresh, calls, checks)
			}
			if !errors.Is(errRefresh, ErrSIWCRefreshDeferred) {
				t.Fatalf("maintenance wake error %v did not wrap ErrSIWCRefreshDeferred", errRefresh)
			}
			if tc.probeErr != nil && !errors.Is(errRefresh, tc.probeErr) {
				t.Fatalf("power probe error lost: %v", errRefresh)
			}

			currentErr = nil
			currentState = "Current System Capabilities are: CPU Graphics Audio Network\nCurrent Power State: 4\n"
			token, errRefresh = auth.RefreshTokensWithRetry(context.Background(), "ordinary-sleep-refresh", 3)
			if errRefresh != nil || token == nil || token.AccessToken != "ordinary-new-access" || token.RefreshToken != "ordinary-new-refresh" || calls != 1 {
				t.Fatalf("interactive wake renewal failed: token=%+v, err=%v, calls=%d", token, errRefresh, calls)
			}
		})
	}
}
