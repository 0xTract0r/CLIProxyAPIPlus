package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeNullCacheControlIgnored(t *testing.T) {
	// 1 real cache_control followed by 5 "cache_control": null blocks.
	payload := []byte(`{
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "block 0", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "block 1", "cache_control": null},
					{"type": "text", "text": "block 2", "cache_control": null},
					{"type": "text", "text": "block 3", "cache_control": null},
					{"type": "text", "text": "block 4", "cache_control": null},
					{"type": "text", "text": "block 5", "cache_control": null}
				]
			}
		]
	}`)

	// countCacheControls must count only valid breakpoints, ignoring null.
	if got := countCacheControls(payload); got != 1 {
		t.Fatalf("countCacheControls = %d, want 1 (null cache_control must not be counted)", got)
	}

	// enforceCacheControlLimit must not strip the real cache breakpoint when nulls are present.
	out := enforceCacheControlLimit(payload, 4)
	if got := countCacheControls(out); got != 1 {
		t.Fatalf("countCacheControls after enforce = %d, want 1", got)
	}
	if !ojson.GetBytes(out, "messages.0.content.0.cache_control").Exists() {
		t.Fatalf("real cache_control at messages.0.content.0 was incorrectly stripped")
	}
	if ojson.GetBytes(out, "messages.0.content.0.cache_control.type").String() != "ephemeral" {
		t.Fatalf("messages.0.content.0.cache_control.type = %q, want ephemeral", ojson.GetBytes(out, "messages.0.content.0.cache_control.type").String())
	}

	// Auto-injection must not be suppressed by "cache_control": null in system or tools.
	toolsPayload := []byte(`{
		"tools": [{"name": "tool1", "description": "desc", "cache_control": null}]
	}`)
	injectedTools := injectToolsCacheControl(toolsPayload)
	if got := ojson.GetBytes(injectedTools, "tools.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("injectToolsCacheControl should inject cache_control when existing value is null, got %q", got)
	}

	systemPayload := []byte(`{
		"system": [{"type": "text", "text": "sys", "cache_control": null}]
	}`)
	injectedSystem := injectSystemCacheControl(systemPayload)
	if got := ojson.GetBytes(injectedSystem, "system.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("injectSystemCacheControl should inject cache_control when existing value is null, got %q", got)
	}

	messagesPayload := []byte(`{
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello", "cache_control": null}]}
		]
	}`)
	injectedMessages := injectMessagesCacheControl(messagesPayload)
	if got := ojson.GetBytes(injectedMessages, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("injectMessagesCacheControl should inject cache_control when existing value is null, got %q", got)
	}

	// enforceCacheControlLimit stripping loop when total real marks > 4 and nulls are present.
	excessWithNullPayload := []byte(`{
		"tools": [
			{"name": "t1", "cache_control": {"type": "ephemeral"}},
			{"name": "t2", "cache_control": null},
			{"name": "t3", "cache_control": {"type": "ephemeral"}}
		],
		"system": [
			{"type": "text", "text": "s1", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "s2", "cache_control": null}
		],
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "u1", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "u2", "cache_control": null},
					{"type": "text", "text": "u3", "cache_control": {"type": "ephemeral"}}
				]
			}
		]
	}`)
	// Total real marks = 2 tools + 1 system + 2 messages = 5 real marks (and 3 nulls).
	// Limit 4 means 1 real mark should be stripped (specifically t1, non-last tool).
	outExcess := enforceCacheControlLimit(excessWithNullPayload, 4)
	if got := countCacheControls(outExcess); got != 4 {
		t.Fatalf("countCacheControls after enforce with excess = %d, want 4", got)
	}
	if ojson.GetBytes(outExcess, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !ojson.GetBytes(outExcess, "tools.2.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved")
	}

	// normalizeCacheControlTTL must not treat "cache_control": null as a 5m breakpoint that downgrades 1h TTL.
	mixedTTLPayload := []byte(`{
		"system": [
			{"type": "text", "text": "sys0", "cache_control": null},
			{"type": "text", "text": "sys1", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
		]
	}`)
	normalized := normalizeCacheControlTTL(mixedTTLPayload)
	if got := ojson.GetBytes(normalized, "system.1.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("normalizeCacheControlTTL downgraded 1h TTL due to null cache_control: %s", normalized)
	}
}

// Client-supplied system blocks must be text, including when counting locally.
func TestOrdinaryClaudeCountRejectsNonTextSystemBlock(t *testing.T) {
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeSystemRejectRT{t})
	request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"instructions"},{"type":"input_image"}],"messages":[{"role":"user","content":"hello"}]}`)}
	_, err := NewClaudeExecutor(&config.Config{}).CountTokens(ctx, nil, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CountTokens error=%v, want 400", err)
	}
}

type ordinaryClaudeSystemRejectRT struct{ t *testing.T }

func (rt ordinaryClaudeSystemRejectRT) RoundTrip(*http.Request) (*http.Response, error) {
	rt.t.Error("invalid CountTokens request reached the transport")
	return nil, errors.New("unexpected request")
}
