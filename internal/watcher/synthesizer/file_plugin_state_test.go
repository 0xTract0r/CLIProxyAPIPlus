package synthesizer

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"path/filepath"
	"testing"
	"time"
)

func TestOrdinaryPluginFileReopenPreservesOperatorState(t *testing.T) {
	for _, tc := range []struct {
		name, raw   string
		status      coreauth.Status
		quarantined bool
	}{
		{"disabled", `{"type":"sample-plugin","disabled":true,"auto_quarantined":true,"quarantine_reason":"local","reauth_required":true}`, coreauth.StatusDisabled, true},
		{"quarantined", `{"type":"sample-plugin","auto_quarantined":true,"quarantined_at":"2025-01-09T01:02:03Z","quarantine_reason":"local"}`, coreauth.StatusQuarantined, true},
		{"reauth", `{"type":"sample-plugin","refresh_status":"reauth_required","refresh_error_code":"invalid_grant"}`, coreauth.StatusError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sc := &SynthesisContext{Config: &config.Config{}, AuthDir: dir, Now: time.Now(), PluginAuthParser: multiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
				return []*coreauth.Auth{{Provider: "sample-plugin", Status: coreauth.StatusActive}}, true, nil
			})}
			auths, handled, err := SynthesizeAuthFileDetailed(sc, filepath.Join(dir, "sample.json"), []byte(tc.raw))
			if err != nil || !handled || len(auths) != 1 {
				t.Fatalf("parse result %v %d %v", handled, len(auths), err)
			}
			a := auths[0]
			if a.Status != tc.status || a.AutoQuarantined != tc.quarantined || !a.Unavailable {
				t.Fatalf("status=%v quarantined=%v unavailable=%v", a.Status, a.AutoQuarantined, a.Unavailable)
			}
			if tc.quarantined && a.QuarantineReason != "local" {
				t.Fatal("missing quarantine reason")
			}
			if tc.name == "reauth" && !coreauth.IsReauthRequiredMetadata(a.Metadata) {
				t.Fatal("plugin lost terminal state")
			}
		})
	}
}
