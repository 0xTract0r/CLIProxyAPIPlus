package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestApplyClaudeHeaders_NonStructuredOperatorXAppCannotLeakNonCli covers requirement ⑥:
// on the non-structured managed-header path, an operator header:X-App override to a
// non-cli value (e.g. "browser") must not leak. X-App is the de-anonymization anchor
// and is pinned to "cli" on both the structured and non-structured paths. Before the
// fix this path let header:X-App=browser overwrite the forced "cli".
func TestApplyClaudeHeaders_NonStructuredOperatorXAppCannotLeakNonCli(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{
		"X-App": []string{"cli"},
	})
	// Attrs-only auth (no account_settings metadata) -> non-structured path. The
	// operator tries to override X-App to a non-cli value through header:X-App.
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key":      "key-xapp-nonstruct",
			"header:X-App": "browser",
		},
	}
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		t.Fatal("test setup error: auth should take the non-structured managed-header path")
	}

	applyClaudeHeaders(req, auth, "key-xapp-nonstruct", false, nil, nil)

	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q (operator header:X-App must not leak a non-cli value)", got, "cli")
	}
}

// TestApplyClaudeHeaders_NonStructuredOperatorOtherHeaderStillOverrides confirms the
// ⑥ fix only pins X-App: other managed headers on the non-structured path still
// honor the operator header:<name> override.
func TestApplyClaudeHeaders_NonStructuredOperatorOtherHeaderStillOverrides(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{
		"X-App": []string{"cli"},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key":                    "key-other-nonstruct",
			"header:X-Stainless-Timeout": "123",
		},
	}
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		t.Fatal("test setup error: auth should take the non-structured managed-header path")
	}

	applyClaudeHeaders(req, auth, "key-other-nonstruct", false, nil, nil)

	if got := req.Header.Get("X-Stainless-Timeout"); got != "123" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q (operator override must still apply)", got, "123")
	}
	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q", got, "cli")
	}
}
