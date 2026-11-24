package executor

import (
	"errors"
	"net/http"
	"testing"
	"time"

	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryAntigravityValidationFailureAppliesToSelectedCredential(t *testing.T) {
	auth, err := executeOrdinaryAntigravityManager(t, http.StatusForbidden, nil,
		`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"validate credential","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED"}]}}`)
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != http.StatusForbidden ||
		!status.IsCredentialScoped() || status.IsTerminalTOSViolation() {
		t.Fatalf("ordinary validation denial lost its exact scope: %v", err)
	}
	if authsdk.IsAuthAvailableForModel(auth, ordinaryAntigravityManagerModel, time.Now()) ||
		authsdk.IsAuthAvailableForModel(auth, "fixture-secondary", time.Now()) {
		t.Fatal("a credential-level denial remained available for another model of that credential")
	}
}
