package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	run "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyIdentity401Boundaries(t *testing.T) {
	type wire struct{ body, header, device string }
	ch := make(chan wire, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u := gjson.GetBytes(b, "metadata.user_id").String()
		ch <- wire{gjson.Get(u, "session_id").String(), r.Header.Get("X-Claude-Code-Session-Id"), gjson.Get(u, "device_id").String()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	e := NewClaudeExecutor(&config.Config{})
	p := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"x"}]}`)
	call := func(key string, body []byte, headers http.Header) wire {
		t.Helper()
		a := &auth.Auth{ID: key, ProxyURL: "direct", Attributes: map[string]string{"api_key": key, "base_url": srv.URL, "cloak_mode": "always"}}
		if _, err := e.Execute(context.Background(), a, run.Request{Model: "claude-opus-4-6", Payload: body}, run.Options{SourceFormat: tr.FormatClaude, Headers: headers}); err != nil {
			t.Fatal(err)
		}
		return <-ch
	}
	a := call("boundary-401-a", p, nil)
	b := call("boundary-401-a", p, nil)
	t.Run("repeat", func(t *testing.T) {
		if a.header != b.header || a.header != helps.CachedSessionID("boundary-401-a") || a.device != b.device || a.body == b.body {
			t.Fatal("F legacy cache/fabrication domains changed")
		}
	})
	c := call("boundary-401-b", p, nil)
	t.Run("key-scope", func(t *testing.T) {
		if a.header == c.header || a.device == c.device || c.header != helps.CachedSessionID("boundary-401-b") {
			t.Fatal("API-key scopes crossed")
		}
	})
	const caller = "11111111-2222-4333-8444-555555555555"
	fake := `{"device_id":"` + strings.Repeat("a", 64) + `","account_uuid":"","session_id":"` + caller + `"}`
	q, _ := sjson.SetBytes(p, "metadata.user_id", fake)
	d := call("boundary-401-a", q, http.Header{"X-Claude-Code-Session-Id": {caller}})
	t.Run("caller-untrusted", func(t *testing.T) {
		if d.device != a.device || d.device == strings.Repeat("a", 64) || d.header != a.header || d.header == caller {
			t.Fatal("caller overrode account device or API-key header session")
		}
		t.Logf("caller_body_session_preserved=%v caller_header_preserved=%v", d.body == caller, d.header == caller)
	})
}
