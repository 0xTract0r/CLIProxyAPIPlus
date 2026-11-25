package management

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryCredentialResendDisabledFalsePreservesExistingState(t *testing.T) {
	auth := &coreauth.Auth{Status: coreauth.StatusError, StatusMessage: "existing failure", Unavailable: true, Metadata: map[string]any{"disabled": false}}
	syncAuthFileDisabledState(auth)
	if auth.Disabled || auth.Status != coreauth.StatusError || auth.StatusMessage != "existing failure" || !auth.Unavailable {
		t.Fatal("unrelated edit reset the previous status")
	}
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	syncAuthFileDisabledState(auth)
	if auth.Disabled || auth.Status != coreauth.StatusActive || auth.StatusMessage != "" {
		t.Fatal("explicit re-enable did not update status")
	}
}
