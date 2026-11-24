package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The server waits for all explicit creates before rejecting one. This makes
// queue ownership deterministic without sleeps or production timeouts.
func TestCodexDuplexRejectedCreateMetadata(t *testing.T) {
	for _, scenario := range []struct {
		name                     string
		activeFailure, ambiguous bool
	}{
		{"rejected_create", false, false}, {"active_failure", true, false}, {"ambiguous_failure", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			activeFailure, ambiguous := scenario.activeFailure, scenario.ambiguous
			wireKeys := make(chan [2]string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
				read := func() string {
					_, p, e := c.ReadMessage()
					if e != nil {
						t.Error(e)
						return ""
					}
					return ojson.GetBytes(p, "prompt_cache_key").String()
				}
				write := func(kind, id, key string) {
					p := fmt.Sprintf(`{"type":%q,"response":{"id":%q,"prompt_cache_key":%q,"output":[],"error":{"type":"invalid_request_error","message":"rejected"}}}`, kind, id, key)
					if e := c.WriteMessage(websocket.TextMessage, []byte(p)); e != nil {
						t.Error(e)
					}
				}
				firstKey := read()
				write("response.created", "first", firstKey)
				rejectedKey, rejectedID := firstKey, "first"
				if !activeFailure {
					write("response.completed", "first", firstKey)
					rejectedKey, rejectedID = read(), "rejected"
				}
				goodKey := read()
				wireKeys <- [2]string{rejectedKey, goodKey}
				if ambiguous {
					write("response.failed", "", rejectedKey)
					_, _, _ = c.ReadMessage()
					return
				}
				write("response.failed", rejectedID, rejectedKey)
				write("response.created", "good", goodKey)
				write("response.completed", "good", goodKey)
				_, _, _ = c.ReadMessage()
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			input := make(chan core.WebsocketInput, 2)
			ctx = fixtureCodexTenantContext(core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input))
			cfg := &config.Config{}
			cfg.Codex.ResponseSteering = true
			cfg.CodexResponseSteering = true
			exec := NewCodexWebsocketsExecutor(cfg)
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			credential := &auth.Auth{ID: "metadata-account", Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": upstream.URL, "websockets": "true"}}
			request := func(key string) []byte {
				return []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-6-astra","prompt_cache_key":%q,"input":[]}`, key))
			}
			result, err := exec.ExecuteStream(ctx, credential, core.Request{Model: "gpt-6-astra", Payload: request("first-key")}, core.Options{SourceFormat: translator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name()}})
			if err != nil {
				t.Fatal(err)
			}
			sent, failed, completed, terminated := false, false, false, false
			var actual [2]string
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					var scoped interface{ IsRequestScoped() bool }
					if !ambiguous || !errors.As(chunk.Err, &scoped) || !scoped.IsRequestScoped() {
						t.Fatal(chunk.Err)
					}
					terminated = true
					continue
				}
				kind := ojson.GetBytes(chunk.Payload, "type").String()
				id := ojson.GetBytes(chunk.Payload, "response.id").String()
				if !sent && id == "first" && ((activeFailure && kind == "response.created") || (!activeFailure && kind == "response.completed")) {
					sent = true
					if !activeFailure {
						input <- core.WebsocketInput{Payload: request("rejected-key")}
					}
					input <- core.WebsocketInput{Payload: request("good-key")}
				}
				if kind == "response.failed" {
					failed = true
					if ambiguous {
						continue
					}
					actual = <-wireKeys
					want := actual[0]
					if want == "" || actual[1] == "" || want == actual[1] {
						t.Fatal("prepared wire keys must be nonempty and distinct for the two creates")
					}
					if got := ojson.GetBytes(chunk.Payload, "response.prompt_cache_key").String(); got != want {
						t.Errorf("failure metadata = %q, want %q", got, want)
					}
				}
				if kind == "response.completed" && id == "good" {
					completed = true
					if got := ojson.GetBytes(chunk.Payload, "response.prompt_cache_key").String(); got != actual[1] {
						t.Errorf("success consumed another create's metadata: %q", got)
					}
					cancel()
				}
			}
			if ambiguous {
				if !failed || !terminated || completed {
					t.Fatalf("failed=%t terminated=%t completed=%t", failed, terminated, completed)
				}
			} else if !failed || !completed {
				t.Fatalf("failed=%t completed=%t", failed, completed)
			}
		})
	}
}
