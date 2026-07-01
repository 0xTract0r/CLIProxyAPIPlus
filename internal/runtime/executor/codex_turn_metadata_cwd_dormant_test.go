package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// These tests pin the anticorr decision that cwd normalization is DORMANT: the
// codex turn-metadata header cwd/git rewrite (previously unconditional) is now
// gated by config.NormalizeAccountEnvEnabled, which is forced off in LoadConfig.
// So on the serving path the header carries the REAL cwd / git commit / git
// remote through unchanged (透传). The identity fields (installation_id / turn_id
// / session_id / thread_id) are handled by a separate, still-active mechanism and
// must still be confused — that is asserted here too so the two concerns do not
// get conflated.

const (
	dormantRealCwd       = "/Users/realdev/Project/ai/cliproxy-stack/.worktrees/anticorr-hardening"
	dormantRealGitCommit = "e2b18565b7d477866f1bb502d3c017f129f4f03d"
	dormantRealGitRemote = "git@github.com:realorg/cliproxy-stack.git"
	dormantRealInstallID = "6a9aea66-9c05-4a26-8c27-038f82fabaed"
	dormantRealTurnID    = "turn-real-cwd-1"
)

// turnMetadataWithWorkspaces builds a turn-metadata header JSON that exposes the
// real cwd (as the workspaces object KEY), the real git commit and the real git
// remote, plus identity fields, mirroring a real codex client's header.
func turnMetadataWithWorkspaces() string {
	return `{` +
		`"installation_id":"` + dormantRealInstallID + `",` +
		`"turn_id":"` + dormantRealTurnID + `",` +
		`"workspaces":{"` + dormantRealCwd + `":{` +
		`"associated_remote_urls":{"origin":"` + dormantRealGitRemote + `"},` +
		`"latest_git_commit_hash":"` + dormantRealGitCommit + `",` +
		`"has_changes":true}}` +
		`}`
}

func assertHeaderCwdGitPreserved(t *testing.T, where, tm string) {
	t.Helper()
	if !gjson.Get(tm, "workspaces").Exists() {
		t.Fatalf("%s: workspaces missing entirely: %s", where, tm)
	}
	// The real cwd must survive as the workspaces KEY.
	ws := gjson.Get(tm, "workspaces").Map()
	if _, ok := ws[dormantRealCwd]; !ok {
		var keys []string
		for k := range ws {
			keys = append(keys, k)
		}
		t.Fatalf("%s: real cwd key %q not preserved, workspace keys=%v", where, dormantRealCwd, keys)
	}
	if got := ws[dormantRealCwd].Get("latest_git_commit_hash").String(); got != dormantRealGitCommit {
		t.Fatalf("%s: git commit = %q, want real %q (dormant → pass-through)", where, got, dormantRealGitCommit)
	}
	if got := ws[dormantRealCwd].Get("associated_remote_urls.origin").String(); got != dormantRealGitRemote {
		t.Fatalf("%s: git remote = %q, want real %q (dormant → pass-through)", where, got, dormantRealGitRemote)
	}
	// Belt-and-suspenders: the raw header must still contain the literal real path.
	if !strings.Contains(tm, dormantRealCwd) {
		t.Fatalf("%s: real cwd literal %q not present in header, cwd normalization must be dormant: %s", where, dormantRealCwd, tm)
	}
}

// TestCodexTurnMetadataHeader_CwdGitDormant_HTTP asserts the HTTP applyCodexHeaders
// path leaves the real cwd/git in the turn-metadata header unchanged while the
// account-env switch is off (the production state).
func TestCodexTurnMetadataHeader_CwdGitDormant_HTTP(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", turnMetadataWithWorkspaces())
	//nolint:staticcheck // applyCodexHeaders reads the gin context via the "gin" string key.
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	httpReq := httptest.NewRequest("POST", "https://example.com/responses", nil)
	httpReq = httpReq.WithContext(ctx)
	httpReq.Header.Set("X-Codex-Turn-Metadata", turnMetadataWithWorkspaces())

	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-cwd-dormant", Provider: "codex"}
	cfg := &config.Config{} // NormalizeAccountEnv unset -> dormant / off (production state)

	applyCodexHeaders(httpReq, auth, "oauth-token", true, cfg)

	got := httpReq.Header.Get("X-Codex-Turn-Metadata")
	assertHeaderCwdGitPreserved(t, "HTTP header", got)
}

// TestCodexTurnMetadataHeader_CwdGitDormant_WS asserts the same pass-through on the
// websocket applyCodexWebsocketHeaders path (lower-case header key).
func TestCodexTurnMetadataHeader_CwdGitDormant_WS(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	//nolint:staticcheck // applyCodexWebsocketHeaders reads the gin context via the "gin" string key.
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	headers := http.Header{}
	headers.Set("x-codex-turn-metadata", turnMetadataWithWorkspaces())

	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-cwd-dormant-ws", Provider: "codex"}
	cfg := &config.Config{} // dormant / off

	out := applyCodexWebsocketHeaders(ctx, headers, auth, "oauth-token", cfg)

	got := out.Get("X-Codex-Turn-Metadata") // http.Header canonicalizes keys
	assertHeaderCwdGitPreserved(t, "WS header", got)
}
