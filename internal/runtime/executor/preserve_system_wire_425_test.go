package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F synthetic_device_id.go:120–130 and claude_device_profile.go:679–693.
// Independent fixed-salt oracle; no production derivation helper is called.
func configure425SyntheticDevice(t *testing.T, cfg *config.Config, id string) string {
	t.Helper()
	cfg.AuthDir = t.TempDir()
	salt := bytes.Repeat([]byte{0xab}, 32)
	if err := os.WriteFile(filepath.Join(cfg.AuthDir, ".synthetic-device-salt"), []byte(hex.EncodeToString(salt)), 0600); err != nil {
		t.Fatal(err)
	}
	preimage := append(append([]byte{}, salt...), []byte("\x00auth:"+id)...)
	sum := sha256.Sum256(preimage)
	return hex.EncodeToString(sum[:])
}
func TestPreserveSystem425AdditionalBoundaries(t *testing.T) {
	for _, mode := range []string{"header-spoof", "uncloak", "auto-native", "strict-config", "farm-config", "config-nil"} {
		t.Run(mode, func(t *testing.T) {
			a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-425", "cloak_mode": "always"}, Metadata: map[string]any{}}
			cfg := &config.Config{}
			confirmed := false
			want := false
			ctx := helps.WithIncomingHeaders(context.Background(), http.Header{"Cloak-Preserve-Caller-System": {"true"}, "X-CPA-Claude-Preserve-Caller-System": {"true"}})
			switch mode {
			case "uncloak":
				a.Attributes["cloak_mode"] = "uncloak"
				want = true
			case "auto-native":
				a.Attributes["cloak_mode"] = "auto"
				confirmed = true
				want = true
			case "strict-config", "farm-config", "config-nil":
				a.Attributes["cloak_preserve_caller_system"] = "true"
				v := true
				c := &config.CloakConfig{PreserveCallerSystem: &v}
				cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "sk-ant-oat-425", Cloak: c}}
				if mode == "strict-config" {
					c.StrictMode = true
				} else if mode == "farm-config" {
					a.Metadata[auth.FarmEnrolledMetadataKey] = true
				} else {
					c.PreserveCallerSystem = nil
					want = true
				}
			}
			out, _, err := applyCloaking(ctx, cfg, a, []byte(`{"model":"claude-opus-5","system":"EXTRA425 task","messages":[{"role":"user","content":"hello"}]}`), "sk-ant-oat-425", confirmed, true)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "EXTRA425") != want {
				t.Fatalf("mode=%s retention=%v want=%v", mode, strings.Contains(string(out), "EXTRA425"), want)
			}
		})
	}
}
