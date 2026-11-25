package executor

import (
	"errors"
	"net/http"
	"testing"
	"time"

	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityTOSStatusErrTerminal(t *testing.T) {
	auth, err := executeOrdinaryAntigravityManager(t, http.StatusForbidden, nil,
		`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"ordinary provider policy denial","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`)
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != http.StatusForbidden ||
		!status.IsTerminalTOSViolation() || !status.IsCredentialScoped() {
		t.Fatalf("explicit provider policy denial lost its terminal indication: %v", err)
	}
	if !auth.Disabled || authsdk.IsAuthAvailableForModel(auth, ordinaryAntigravityManagerModel, time.Now().Add(24*time.Hour)) {
		t.Fatal("terminal provider decision was reduced to a timed wait")
	}
}
