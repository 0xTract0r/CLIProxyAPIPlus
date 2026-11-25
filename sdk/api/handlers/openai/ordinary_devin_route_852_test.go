package openai

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestOrdinaryDevinPublishedPinnedRoute(t *testing.T) {
	const alias = "fixture-devin-collision(high)"
	const base = "fixture-devin-collision"
	const native = "devin/swe-1-6-slow"
	m := coreauth.NewManager(nil, nil, nil)
	first := &coreauth.Auth{ID: t.Name() + "/selected", Provider: "devin", Status: coreauth.StatusActive}
	other := &coreauth.Auth{ID: t.Name() + "/base-only", Provider: "devin", Status: coreauth.StatusActive}
	r := registry.GetGlobalRegistry()
	r.RegisterClient(first.ID, "devin", []*registry.ModelInfo{{ID: alias, MetadataModelID: native, OAuthAliasSource: native}})
	r.RegisterClient(other.ID, "devin", []*registry.ModelInfo{{ID: base, MetadataModelID: "devin/swe-1-5"}})
	t.Cleanup(func() { r.UnregisterClient(first.ID); r.UnregisterClient(other.ID) })
	if got := m.ResolveExecutionModel(first, alias); got != native+"(high)" {
		t.Errorf("selected route resolved to %q", got)
	}
	if got := m.ResolveExecutionModel(other, alias); got != "" {
		t.Errorf("base-only credential acquired exact route %q", got)
	}
	if !responsesWebsocketPinnedAuthMatchesModel(first, alias, base, false) {
		t.Error("exact registered credential could not retain pin")
	}
	if responsesWebsocketPinnedAuthMatchesModel(other, alias, base, false) {
		t.Error("base-only credential could acquire the pin")
	}
	if !responsesWebsocketPinnedAuthMatchesModel(other, base+"(low)", base, false) {
		t.Error("an unshadowed thinking suffix stopped working")
	}
	first.ModelStates = map[string]*coreauth.ModelState{native: {Unavailable: true, NextRetryAfter: time.Now().Add(time.Minute)}}
	if responsesWebsocketPinnedAuthMatchesModel(first, alias, base, false) {
		t.Error("alias lost the selected credential's native cooldown")
	}
}

type ordinaryDevinExecutor struct {
	coreauth.ProviderExecutor
	call func(*coreauth.Auth, executorapi.Request) (*executorapi.StreamResult, error)
}

func (ordinaryDevinExecutor) Identifier() string { return "devin" }
func (e ordinaryDevinExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, req executorapi.Request, _ executorapi.Options) (*executorapi.StreamResult, error) {
	return e.call(a, req)
}

func TestOrdinaryDevinSelectedCredentialStream(t *testing.T) {
	const alias = "ordinary-devin-collision(high)"
	const native = "devin/swe-1-6-slow"
	m := coreauth.NewManager(nil, nil, nil)
	selected := &coreauth.Auth{ID: t.Name() + "/selected", Provider: "devin", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{
		alias: {Unavailable: true, NextRetryAfter: time.Now().Add(time.Minute)},
	}}
	other := &coreauth.Auth{ID: t.Name() + "/other", Provider: "devin", Status: coreauth.StatusActive}
	reg := registry.GetGlobalRegistry()
	models := []*registry.ModelInfo{{ID: alias, MetadataModelID: native, OAuthAliasSource: native}}
	for _, a := range []*coreauth.Auth{selected, other} {
		if _, err := m.Register(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(a.ID, "devin", models)
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	}
	m.ReconcileRegistryModelStates(t.Context(), selected.ID)
	loaded, ok := m.GetByID(selected.ID)
	if !ok || loaded.ModelStates[native] == nil || !loaded.ModelStates[native].Unavailable {
		t.Error("legacy exact alias cooldown was not preserved on native key")
	}
	calls := 0
	fixture := ordinaryDevinExecutor{call: func(a *coreauth.Auth, req executorapi.Request) (*executorapi.StreamResult, error) {
		calls++
		if a.ID != selected.ID || req.Model != native+"(high)" {
			t.Errorf("stream used unexpected credential or model %q %q", a.ID, req.Model)
		}
		chunks := make(chan executorapi.StreamChunk, 1)
		chunks <- executorapi.StreamChunk{Payload: []byte("ordinary stream")}
		close(chunks)
		return &executorapi.StreamResult{Chunks: chunks}, nil
	}}
	m.RegisterExecutor(fixture)
	req := executorapi.Request{Model: alias}
	opts := executorapi.Options{Metadata: map[string]any{executorapi.PinnedAuthMetadataKey: selected.ID}}
	if _, err := m.ExecuteStream(t.Context(), []string{"devin"}, req, opts); err == nil || calls != 0 {
		t.Fatalf("cooldown should reject the pinned credential without another execution: err=%v calls=%d", err, calls)
	}
	ready := coreauth.NewManager(nil, nil, nil)
	ready.RegisterExecutor(fixture)
	selected.ModelStates = nil
	for _, a := range []*coreauth.Auth{selected, other} {
		if _, err := ready.Register(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ready.ExecuteStream(t.Context(), []string{"devin"}, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var data string
	for chunk := range got.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		data += string(chunk.Payload)
	}
	if data != "ordinary stream" || calls != 1 {
		t.Errorf("incomplete stream or repeated attempt: data=%q calls=%d", data, calls)
	}
}
