package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"testing"

	"github.com/MoeForge/moego/ojson"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
)

func TestOrdinaryClaudeCacheTTLAndLimit(t *testing.T) {
	const withMarkers = `{"thread":{},"tools":[{"name":"first","cache_control":{"type":"ephemeral","ttl":"1h"}},{"name":"second","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"older","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"latest","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	for _, explicit := range []bool{false, true} {
		d := ojson.NewDocString(withMarkers)
		normalizeOrdinaryClaudeCache(d, explicit)
		if d.Get("tools.0.cache_control").Exists() || d.Get("messages.0.content.0.cache_control").Exists() {
			t.Fatalf("excess marker order %s", d.Bytes())
		}
		if d.Get("tools.1.cache_control.ttl").String() != "1h" || !d.Get("system.0.cache_control").Exists() || !d.Get("messages.0.content.1.cache_control").Exists() {
			t.Fatalf("permitted markers removed %s", d.Bytes())
		}
		if got := d.Get("messages.0.content.1.cache_control.ttl").Exists(); got != explicit {
			t.Fatalf("explicit=%v ttlExists=%v", explicit, got)
		}
	}
}

func TestOrdinaryClaudeUnsupportedModeIsRejected(t *testing.T) {
	e := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "fingerprint_profile": "unsupported"}}
	r, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := e.PrepareRequest(r, auth).(statusErr)
	if !ok || got.StatusCode() != 400 || !got.IsRequestScoped() || r.Header.Get("X-Api-Key") != "" {
		t.Fatalf("unexpected response %#v", got)
	}
}

func TestOrdinaryClaudeSharedCacheHelpers(t *testing.T) {
	doc := ojson.NewDocString(`{"system":"existing system","tools":[{"name":"weather","input_schema":{"type":"object"}},{"name":"deferred","defer_loading":true}],"messages":[{"role":"user","content":"existing message"}]}`)
	if !shouldEnsureCacheControlDoc(doc, false, false) || !injectToolsCacheControlDoc(doc) || !injectSystemCacheControlDoc(doc) || !injectMessagesCacheControlDoc(doc) {
		t.Fatalf("ordinary Copilot cache helper calls not preserved: %s", doc.Bytes())
	}
	if doc.Get("tools.0.cache_control.type").String() != "ephemeral" || doc.Get("tools.1.cache_control").Exists() || doc.Get("system.0.text").String() != "existing system" || doc.Get("messages.0.content.0.text").String() != "existing message" {
		t.Fatalf("ordinary cache insertion altered user fields: %s", doc.Bytes())
	}
	if shouldEnsureCacheControlDoc(doc, false, false) {
		t.Fatal("existing markers must suppress defaults")
	}
}
