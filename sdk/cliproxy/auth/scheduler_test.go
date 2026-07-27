package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type schedulerTestExecutor struct {
	provider string
}

func (e schedulerTestExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "test"
}

func (schedulerTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (schedulerTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (schedulerTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type retryAfterStatusErr struct {
	code       int
	msg        string
	retryAfter time.Duration
}

func (e retryAfterStatusErr) Error() string { return e.msg }

func (e retryAfterStatusErr) StatusCode() int { return e.code }

func (e retryAfterStatusErr) RetryAfter() *time.Duration {
	if e.retryAfter <= 0 {
		return nil
	}
	value := e.retryAfter
	return &value
}

type quotaHeaderExecutor struct {
	headers http.Header
}

func (e quotaHeaderExecutor) Identifier() string { return "codex" }

func (e quotaHeaderExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{}`), Headers: e.headers.Clone()}, nil
}

func (e quotaHeaderExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: e.headers.Clone(), Chunks: chunks}, nil
}

func (e quotaHeaderExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{}`), Headers: e.headers.Clone()}, nil
}

func (e quotaHeaderExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e quotaHeaderExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: e.headers.Clone(), Body: http.NoBody}, nil
}

type fakePluginScheduler struct {
	resp     pluginapi.SchedulerPickResponse
	handled  bool
	err      error
	calls    int
	requests []pluginapi.SchedulerPickRequest
	pick     func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

func (s *fakePluginScheduler) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.calls++
	s.requests = append(s.requests, req)
	if s.pick != nil {
		return s.pick(ctx, req)
	}
	return s.resp, s.handled, s.err
}

type inactivePluginScheduler struct {
	fakePluginScheduler
}

type authKindHomeDispatcher struct {
	auths  []Auth
	counts []int
}

func (d *authKindHomeDispatcher) HeartbeatOK() bool {
	return true
}

func (d *authKindHomeDispatcher) RPopAuth(_ context.Context, _ string, _ string, _ http.Header, count int) ([]byte, error) {
	d.counts = append(d.counts, count)
	if count < 1 || count > len(d.auths) {
		return nil, home.ErrAuthNotFound
	}
	return json.Marshal(homeAuthDispatchResponse{Auth: d.auths[count-1]})
}

func (*authKindHomeDispatcher) AbortAmbiguousDispatch() {}

func (s *inactivePluginScheduler) HasScheduler() bool {
	return false
}

type trackingSelector struct {
	calls      int
	lastAuthID []string
}

func (s *trackingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	s.calls++
	s.lastAuthID = s.lastAuthID[:0]
	for _, auth := range auths {
		s.lastAuthID = append(s.lastAuthID, auth.ID)
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[len(auths)-1], nil
}

func newSchedulerForTest(selector Selector, auths ...*Auth) *authScheduler {
	scheduler := newAuthScheduler(selector)
	// These scheduler tests exercise rotation/cooldown semantics, not the missing
	// proxy_url guard. Treat them as running with a global proxy configured so auths
	// without an explicit per-account proxy_url stay schedulable.
	scheduler.setGlobalProxyConfigured(true)
	scheduler.rebuild(auths)
	return scheduler
}

func registerSchedulerModels(t *testing.T, provider string, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})
}

func exhaustedCodexHeaders(resetAt time.Time) http.Header {
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "100")
	headers.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(resetAt.Unix(), 10))
	return headers
}

func TestScheduler_SkipsAuthWithoutProxyURLWhenNoGlobalProxy(t *testing.T) {
	t.Parallel()

	registerSchedulerModels(t, "claude", "claude-3", "with-proxy", "no-proxy")

	scheduler := newAuthScheduler(&RoundRobinSelector{})
	// No global proxy configured (default false): an account without proxy_url has no
	// safe egress path and must be dropped from scheduling.
	scheduler.rebuild([]*Auth{
		{ID: "with-proxy", Provider: "claude", ProxyURL: "http://acc-proxy:8080"},
		{ID: "no-proxy", Provider: "claude"},
	})

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		got, errPick := scheduler.pickSingle(context.Background(), "claude", "claude-3", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", i, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", i)
		}
		seen[got.ID] = true
	}
	if seen["no-proxy"] {
		t.Fatal("auth without proxy_url must not be scheduled when no global proxy is configured")
	}
	if !seen["with-proxy"] {
		t.Fatal("auth with proxy_url should remain schedulable")
	}
}

func TestScheduler_KeepsAuthWithoutProxyURLWhenGlobalProxyConfigured(t *testing.T) {
	t.Parallel()

	registerSchedulerModels(t, "claude", "claude-3", "no-proxy-global")

	scheduler := newAuthScheduler(&RoundRobinSelector{})
	// A global proxy fallback exists, so an account without a per-account proxy_url
	// still has a safe egress path and remains schedulable.
	scheduler.setGlobalProxyConfigured(true)
	scheduler.rebuild([]*Auth{
		{ID: "no-proxy-global", Provider: "claude"},
	})

	got, errPick := scheduler.pickSingle(context.Background(), "claude", "claude-3", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "no-proxy-global" {
		t.Fatalf("expected no-proxy-global to be schedulable with a global proxy, got %v", got)
	}
}

func TestScheduler_GlobalProxyToggleReevaluatesSchedulability(t *testing.T) {
	t.Parallel()

	registerSchedulerModels(t, "claude", "claude-3", "toggle-auth")

	scheduler := newAuthScheduler(&RoundRobinSelector{})
	scheduler.setGlobalProxyConfigured(true)
	scheduler.rebuild([]*Auth{
		{ID: "toggle-auth", Provider: "claude"},
	})

	if got, errPick := scheduler.pickSingle(context.Background(), "claude", "claude-3", cliproxyexecutor.Options{}, nil); errPick != nil || got == nil {
		t.Fatalf("auth should be schedulable while global proxy is on (got=%v err=%v)", got, errPick)
	}

	// Removing the global proxy must drop the now-unprotected proxy-less account.
	scheduler.setGlobalProxyConfigured(false)
	if _, errPick := scheduler.pickSingle(context.Background(), "claude", "claude-3", cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatal("auth without proxy_url must become unschedulable after the global proxy is removed")
	}
}

func TestSchedulerPick_RoundRobinHighestPriority(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "high-b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "high-a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
	)

	want := []string{"high-a", "high-b", "high-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_FillFirstSticksToFirstReady(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "b", Provider: "gemini"},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "c", Provider: "gemini"},
	)

	for index := 0; index < 3; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != "a" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, "a")
		}
	}
}

func TestSchedulerPick_PromotesExpiredCooldownBeforePick(t *testing.T) {
	t.Parallel()

	model := "gemini-2.5-pro"
	registerSchedulerModels(t, "gemini", model, "cooldown-expired")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       "cooldown-expired",
			Provider: "gemini",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(-1 * time.Second),
				},
			},
		},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() auth = nil")
	}
	if got.ID != "cooldown-expired" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "cooldown-expired")
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex"},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_XAIWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "xai-http", Provider: "xai"},
		&Auth{ID: "xai-ws-a", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "xai-ws-b", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"xai-ws-a", "xai-ws-b", "xai-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "xai", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledAcrossPriorities(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestManagerExecute_ClaudeOpusSkipsProEvenWithStaleRegistry(t *testing.T) {
	ctx := context.Background()
	model := "claude-opus-4-7"
	registerSchedulerModels(t, "claude", model, "claude-pro-stale", "claude-max-stale")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{ID: "claude-pro-stale", Provider: "claude", Attributes: map[string]string{"plan_type": "pro"}},
		{ID: "claude-max-stale", Provider: "claude", Attributes: map[string]string{"plan_type": "max"}},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	selectedAuthID := ""
	meta := map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = authID
		},
	}
	if _, errExec := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: meta}); errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if selectedAuthID != "claude-max-stale" {
		t.Fatalf("selected auth = %q, want claude-max-stale", selectedAuthID)
	}
}

func TestManagerExecute_ClaudeOpusAllowsReauthRequiredLastKnownMaxPlan(t *testing.T) {
	ctx := context.Background()
	model := "claude-opus-4-7"
	registerSchedulerModels(t, "claude", model, "claude-reauth-max-stale")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{
			ID:       "claude-reauth-max-stale",
			Provider: "claude",
			Metadata: map[string]any{
				"quota_refresh_status": "reauth_required",
				"plan_type":            "max",
			},
		},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	selectedAuthID := ""
	meta := map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = authID
		},
	}
	if _, errExec := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: meta}); errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if selectedAuthID != "claude-reauth-max-stale" {
		t.Fatalf("selected auth = %q, want claude-reauth-max-stale", selectedAuthID)
	}
}

func TestManagerExecute_ClaudeOpusRejectsProOnlyStaleRegistry(t *testing.T) {
	ctx := context.Background()
	model := "claude-opus-4-7"
	registerSchedulerModels(t, "claude", model, "claude-pro-only-stale")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(ctx, &Auth{
		ID:       "claude-pro-only-stale",
		Provider: "claude",
		Attributes: map[string]string{
			"plan_type":           "pro",
			"extra_usage_enabled": "true",
		},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	_, errExec := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExec == nil {
		t.Fatal("Execute() error = nil, want auth_not_found")
	}
	var authErr *Error
	if !errors.As(errExec, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("Execute() error = %v, want auth_not_found", errExec)
	}
}

func TestManagerExecute_ClaudeOpusLegacySelectorFiltersProBeforePick(t *testing.T) {
	ctx := context.Background()
	model := "claude-opus-4-7"
	registerSchedulerModels(t, "claude", model, "claude-pro-legacy-stale")

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(ctx, &Auth{
		ID:       "claude-pro-legacy-stale",
		Provider: "claude",
		Attributes: map[string]string{
			"plan_type":           "pro",
			"extra_usage_enabled": "true",
		},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	_, errExec := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExec == nil {
		t.Fatal("Execute() error = nil, want auth_not_found")
	}
	var authErr *Error
	if !errors.As(errExec, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("Execute() error = %v, want auth_not_found", errExec)
	}
	if selector.calls != 0 {
		t.Fatalf("selector calls = %d, want 0 because policy filtered candidate before Pick", selector.calls)
	}
}

func TestManagerExecute_CodexSparkSkipsPlusAndUnknownEvenWithStaleRegistry(t *testing.T) {
	ctx := context.Background()
	model := "gpt-5.3-codex-spark"
	registerSchedulerModels(t, "codex", model, "codex-plus-stale", "codex-unknown-stale", "codex-pro-stale")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{ID: "codex-plus-stale", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: "codex-unknown-stale", Provider: "codex"},
		{ID: "codex-pro-stale", Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	selectedAuthID := ""
	meta := map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = authID
		},
	}
	if _, errExec := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: meta}); errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if selectedAuthID != "codex-pro-stale" {
		t.Fatalf("selected auth = %q, want codex-pro-stale", selectedAuthID)
	}
}

func TestManagerExecute_CodexSparkAllowsReauthRequiredLastKnownProPlan(t *testing.T) {
	ctx := context.Background()
	model := "gpt-5.3-codex-spark"
	registerSchedulerModels(t, "codex", model, "codex-reauth-pro-stale")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{
			ID:       "codex-reauth-pro-stale",
			Provider: "codex",
			Metadata: map[string]any{
				"quota_refresh_status": "reauth_required",
				"plan_type":            "pro",
			},
		},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	selectedAuthID := ""
	meta := map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = authID
		},
	}
	if _, errExec := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: meta}); errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if selectedAuthID != "codex-reauth-pro-stale" {
		t.Fatalf("selected auth = %q, want codex-reauth-pro-stale", selectedAuthID)
	}
}

func TestManagerExecute_CodexSparkRejectsPlusAndUnknownOnlyStaleRegistry(t *testing.T) {
	ctx := context.Background()
	model := "gpt-5.3-codex-spark"
	registerSchedulerModels(t, "codex", model, "codex-plus-only-stale", "codex-unknown-only-stale")

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{ID: "codex-plus-only-stale", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: "codex-unknown-only-stale", Provider: "codex"},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	_, errExec := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExec == nil {
		t.Fatal("Execute() error = nil, want auth_not_found")
	}
	var authErr *Error
	if !errors.As(errExec, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("Execute() error = %v, want auth_not_found", errExec)
	}
	if selector.calls != 0 {
		t.Fatalf("selector calls = %d, want 0 because policy filtered candidate before Pick", selector.calls)
	}
}

func TestSchedulerPick_MixedProvidersUsesWeightedProviderRotationOverReadyCandidates(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini"},
		&Auth{ID: "gemini-b", Provider: "gemini"},
		&Auth{ID: "claude-a", Provider: "claude"},
	)

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestSchedulerPick_MixedProvidersPrefersHighestPriorityTier(t *testing.T) {
	t.Parallel()

	model := "gpt-default"
	registerSchedulerModels(t, "provider-low", model, "low")
	registerSchedulerModels(t, "provider-high-a", model, "high-a")
	registerSchedulerModels(t, "provider-high-b", model, "high-b")

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "provider-low", Attributes: map[string]string{"priority": "4"}},
		&Auth{ID: "high-a", Provider: "provider-high-a", Attributes: map[string]string{"priority": "7"}},
		&Auth{ID: "high-b", Provider: "provider-high-b", Attributes: map[string]string{"priority": "7"}},
	)

	providers := []string{"provider-low", "provider-high-a", "provider-high-b"}
	wantProviders := []string{"provider-high-a", "provider-high-b", "provider-high-a", "provider-high-b"}
	wantIDs := []string{"high-a", "high-b", "high-a", "high-b"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), providers, model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_UsesWeightedProviderRotationBeforeCredentialRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, map[string]struct{}{})
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_DisallowFreeAuthSkipsCodexFreePlan(t *testing.T) {
	t.Parallel()

	model := "gpt-5.4-mini"
	registerSchedulerModels(t, "codex", model, "codex-a-free", "codex-b-plus")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}); errRegister != nil {
		t.Fatalf("Register(codex-a-free) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "codex-b-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}); errRegister != nil {
		t.Fatalf("Register(codex-b-plus) error = %v", errRegister)
	}

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true},
	}
	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"codex"}, model, opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "codex" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "codex")
	}
	if got.ID != "codex-b-plus" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "codex-b-plus")
	}
}

func TestManagerPluginSchedulerSelectsAuthID(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-b"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{Stream: true}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if got.ID != "auth-b" {
		t.Fatalf("pickNext() auth.ID = %q, want %q", got.ID, "auth-b")
	}
	if scheduler.calls != 1 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 1)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want %d", len(scheduler.requests), 1)
	}
	if !scheduler.requests[0].Stream {
		t.Fatalf("scheduler request Stream = false, want true")
	}
}

func TestManagerSelectAuthByKindSkipsAPIKey(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, candidate := range []*Auth{
		{ID: "codex-api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "test-key"}},
		{ID: "codex-oauth", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "codex-api-key"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", AuthKindOAuth, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuthByKind() error = %v", errSelect)
	}
	if selected == nil || selected.ID != "codex-oauth" {
		t.Fatalf("SelectAuthByKind() auth = %#v, want codex-oauth", selected)
	}
	if scheduler.calls != 2 {
		t.Fatalf("scheduler.calls = %d, want 2", scheduler.calls)
	}
}

func TestManagerSelectAuthByKindReturnsErrorWhenUnavailable(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:         "codex-api-key",
		Provider:   "codex",
		Attributes: map[string]string{AttributeAPIKey: "test-key"},
	}); errRegister != nil {
		t.Fatalf("Register(codex-api-key) error = %v", errRegister)
	}

	selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", AuthKindOAuth, cliproxyexecutor.Options{})
	if selected != nil {
		t.Fatalf("SelectAuthByKind() auth = %#v, want nil", selected)
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("SelectAuthByKind() error = %#v, want auth_not_found", errSelect)
	}
}

func TestManagerSelectAuthByKindRejectsInvalidKind(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", "certificate", cliproxyexecutor.Options{})
	if selected != nil {
		t.Fatalf("SelectAuthByKind() auth = %#v, want nil", selected)
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != "invalid_auth_kind" || authErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("SelectAuthByKind() error = %#v, want invalid_auth_kind", errSelect)
	}
}

func TestManagerLegacySelectAuthFailsClosedWhenHomeEnabled(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{
		ID:       "home-oauth",
		Provider: "test",
		Metadata: map[string]any{"access_token": "test-token"},
	}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = oldCurrentHomeDispatcher })

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{})

	for name, selectAuth := range map[string]func() (*Auth, error){
		"SelectAuth": func() (*Auth, error) {
			return manager.SelectAuth(context.Background(), "test", "model", cliproxyexecutor.Options{})
		},
		"SelectAuthByKind": func() (*Auth, error) {
			return manager.SelectAuthByKind(context.Background(), "test", "model", AuthKindOAuth, cliproxyexecutor.Options{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			selected, errSelect := selectAuth()
			if selected != nil {
				t.Fatalf("%s() auth = %#v, want nil", name, selected)
			}
			var authErr *Error
			if !errors.As(errSelect, &authErr) || authErr.Code != "home_unavailable" || authErr.HTTPStatus != http.StatusServiceUnavailable {
				t.Fatalf("%s() error = %#v, want home_unavailable", name, errSelect)
			}
		})
	}
	if len(dispatcher.counts) != 0 {
		t.Fatalf("legacy selection issued Home RPOP calls: %v", dispatcher.counts)
	}
}

func TestSelectHomeAuthByKindReturnsHomeSelection(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{
		ID:       "home-oauth",
		Provider: "test",
		Metadata: map[string]any{"access_token": "test-token"},
	}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{})

	selection, errSelect := manager.SelectHomeAuthByKind(context.Background(), "test", "gpt-5.4", AuthKindOAuth, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "home-oauth" {
		t.Fatalf("SelectHomeAuthByKind() = %#v, want home-oauth", selection)
	}
	if selection.Executor == nil || selection.Provider != "test" {
		t.Fatalf("selection executor/provider = %#v/%q, want test", selection.Executor, selection.Provider)
	}
	selection.End("test_complete")
}

func TestSelectHomeAuthByKindSkipsProviderMismatch(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{
		{ID: "wrong-provider", Provider: "other", Metadata: map[string]any{"access_token": "test-token"}},
		{ID: "matching-provider", Provider: "test", Metadata: map[string]any{"access_token": "test-token"}},
	}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{})
	manager.RegisterExecutor(schedulerTestExecutor{provider: "other"})

	selection, errSelect := manager.SelectHomeAuthByKind(context.Background(), "test", "gpt-5.4", AuthKindOAuth, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "matching-provider" {
		t.Fatalf("SelectHomeAuthByKind() = %#v, want matching provider auth", selection)
	}
	if got := dispatcher.counts; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("home auth counts = %v, want [1 2]", got)
	}
	selection.End("test_complete")
}

func TestSelectHomeAuthByKindKeepsLogicalProviderWhenUsingCompatibilityExecutor(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{
		ID:       "compat-auth",
		Provider: "base-url-provider",
		Attributes: map[string]string{
			"base_url":      "https://compat.example.com",
			AttributeAPIKey: "test-key",
		},
	}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{provider: "openai-compatibility"})

	selection, errSelect := manager.SelectHomeAuthByKind(context.Background(), "base-url-provider", "gpt-5.4", AuthKindAPIKey, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "compat-auth" {
		t.Fatalf("SelectHomeAuthByKind() = %#v, want compat-auth", selection)
	}
	if selection.Provider != "base-url-provider" {
		t.Fatalf("selection.Provider = %q, want logical provider base-url-provider", selection.Provider)
	}
	if selection.Executor == nil || selection.Executor.Identifier() != "openai-compatibility" {
		t.Fatalf("selection.Executor = %#v, want openai-compatibility", selection.Executor)
	}
	selection.End("test_complete")
}

func TestPickNextViaHomeEndsPendingOnInvalidAuth(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{Provider: "test"}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	manager.SetHomeExecutionRegistry(registry)
	manager.RegisterExecutor(schedulerTestExecutor{})

	_, _, _, errPick := manager.pickNextViaHome(context.Background(), "gpt-5.4", cliproxyexecutor.Options{}, nil)
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr.Code != "invalid_auth" {
		t.Fatalf("pickNextViaHome() error = %v, want invalid_auth", errPick)
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if errDrain := registry.Drain(drainCtx); errDrain != nil {
		t.Fatalf("Drain() error = %v, pending dispatch was not ended", errDrain)
	}
}

func TestManagerPluginSchedulerSkippedWhenHomeEnabled(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	_, _, _ = manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)

	if scheduler.calls != 0 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 0)
	}
}

func TestManagerInactivePluginSchedulerKeepsFastPath(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	scheduler := &inactivePluginScheduler{}
	manager.SetPluginScheduler(scheduler)

	gotA, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() first error = %v", errPick)
	}
	gotB, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() second error = %v", errPick)
	}
	if gotA == nil || gotB == nil {
		t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotA, gotB)
	}
	if gotA.ID != "auth-a" || gotB.ID != "auth-b" {
		t.Fatalf("fast path picks = %q, %q; want auth-a, auth-b", gotA.ID, gotB.ID)
	}
	if scheduler.calls != 0 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 0)
	}
}

func TestManagerPluginSchedulerCalledOutsideManagerLock(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		handled: true,
		pick: func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
			if !manager.mu.TryLock() {
				t.Fatalf("plugin scheduler called while manager lock is held")
			}
			manager.mu.Unlock()
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"}, true, nil
		},
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if got.ID != "auth-a" {
		t.Fatalf("pickNext() auth.ID = %q, want auth-a", got.ID)
	}
	if scheduler.calls != 1 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 1)
	}
}

func TestManagerPluginSchedulerErrorStopsPick(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		handled: true,
		err:     errors.New("tenant denied"),
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatalf("pickNext() error = nil, want tenant denied")
	}
	if errPick.Error() != "tenant denied" {
		t.Fatalf("pickNext() error = %v, want tenant denied", errPick)
	}
	if got != nil {
		t.Fatalf("pickNext() auth = %v, want nil", got)
	}
}

func TestManagerPluginSchedulerFallsBackWhenUnhandledOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    pluginapi.SchedulerPickResponse
		handled bool
	}{
		{
			name:    "unhandled",
			resp:    pluginapi.SchedulerPickResponse{Handled: false},
			handled: false,
		},
		{
			name:    "unknown auth id",
			resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "missing"},
			handled: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.executors["gemini"] = schedulerTestExecutor{}
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
				t.Fatalf("Register(auth-b) error = %v", errRegister)
			}
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
				t.Fatalf("Register(auth-a) error = %v", errRegister)
			}

			scheduler := &fakePluginScheduler{resp: tc.resp, handled: tc.handled}
			manager.SetPluginScheduler(scheduler)

			got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext() error = %v", errPick)
			}
			if got == nil {
				t.Fatalf("pickNext() auth = nil")
			}
			if got.ID != "auth-a" {
				t.Fatalf("pickNext() auth.ID = %q, want %q", got.ID, "auth-a")
			}
		})
	}
}

func TestManagerPluginSchedulerDelegatesBuiltin(t *testing.T) {
	t.Run("round-robin", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.scheduler.setGlobalProxyConfigured(true)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
			handled: true,
		})

		gotA, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() first error = %v", errPick)
		}
		gotB, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() second error = %v", errPick)
		}
		if gotA == nil || gotB == nil {
			t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotA, gotB)
		}
		if gotA.ID != "auth-a" || gotB.ID != "auth-b" {
			t.Fatalf("round-robin picks = %q, %q; want auth-a, auth-b", gotA.ID, gotB.ID)
		}
	})

	t.Run("round-robin model cursors", func(t *testing.T) {
		reg := registry.GetGlobalRegistry()
		models := []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}}
		for _, authID := range []string{"auth-a", "auth-b"} {
			reg.RegisterClient(authID, "gemini", models)
			t.Cleanup(func() {
				reg.UnregisterClient(authID)
			})
		}

		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.scheduler.setGlobalProxyConfigured(true)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
			handled: true,
		})

		gotModelA, _, errPick := manager.pickNext(context.Background(), "gemini", "model-a", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(model-a) error = %v", errPick)
		}
		gotModelB, _, errPick := manager.pickNext(context.Background(), "gemini", "model-b", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(model-b) error = %v", errPick)
		}
		if gotModelA == nil || gotModelB == nil {
			t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotModelA, gotModelB)
		}
		if gotModelA.ID != "auth-a" || gotModelB.ID != "auth-a" {
			t.Fatalf("model-scoped round-robin picks = %q, %q; want auth-a, auth-a", gotModelA.ID, gotModelB.ID)
		}
	})

	t.Run("fill-first", func(t *testing.T) {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.scheduler.setGlobalProxyConfigured(true)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst},
			handled: true,
		})

		got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() error = %v", errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() auth = nil")
		}
		if got.ID != "auth-a" {
			t.Fatalf("fill-first pick = %q, want auth-a", got.ID)
		}
	})
}

func TestManagerPluginSchedulerDelegateRoundRobinUsesNativeMixedRotation(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}
	manager.SetPluginScheduler(&fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
		handled: true,
	})

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManagerPluginSchedulerPickNextMixedSelectsProvider(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}
	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "claude-a"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	got, executor, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want claude-a", got.ID)
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want claude", provider)
	}
	if executor == nil {
		t.Fatalf("pickNextMixed() executor = nil")
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want %d", len(scheduler.requests), 1)
	}
	req := scheduler.requests[0]
	if req.Provider != "" {
		t.Fatalf("scheduler request Provider = %q, want empty for mixed provider pick", req.Provider)
	}
	if len(req.Providers) != 2 || req.Providers[0] != "gemini" || req.Providers[1] != "claude" {
		t.Fatalf("scheduler request Providers = %#v, want [gemini claude]", req.Providers)
	}
}

func TestManagerInactivePluginSchedulerKeepsMixedFastPath(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	scheduler := &inactivePluginScheduler{}
	manager.SetPluginScheduler(scheduler)

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "gemini" {
		t.Fatalf("pickNextMixed() provider = %q, want gemini", provider)
	}
	if got.ID != "gemini-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want gemini-a", got.ID)
	}
	if scheduler.calls != 0 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 0)
	}
}

func TestManagerPluginSchedulerCandidatesAreSafeCopies(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	auth := &Auth{
		ID:       "auth-a",
		Provider: "gemini",
		Status:   StatusActive,
		Attributes: map[string]string{
			"access_token": "token-value",
			"api_key":      "api-key-value",
			"cookie":       "cookie-value",
			"priority":     "7",
			"team":         "alpha",
		},
		Metadata: map[string]any{"tenant": "one"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		handled: true,
		pick: func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
			if len(req.Candidates) != 1 {
				t.Fatalf("len(req.Candidates) = %d, want %d", len(req.Candidates), 1)
			}
			candidate := req.Candidates[0]
			if candidate.ID != "auth-a" || candidate.Provider != "gemini" || candidate.Priority != 7 || candidate.Status != string(StatusActive) {
				t.Fatalf("scheduler candidate = %#v, want sanitized auth-a metadata", candidate)
			}
			for _, key := range []string{"access_token", "api_key", "cookie"} {
				if _, ok := candidate.Attributes[key]; ok {
					t.Fatalf("scheduler candidate Attributes contains sensitive key %q", key)
				}
			}
			if candidate.Attributes["priority"] != "7" {
				t.Fatalf("scheduler candidate priority attribute = %q, want 7", candidate.Attributes["priority"])
			}
			if len(candidate.Metadata) != 0 {
				t.Fatalf("scheduler candidate Metadata = %#v, want empty", candidate.Metadata)
			}
			candidate.Attributes["team"] = "mutated"
			req.Candidates[0] = candidate
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"}, true, nil
		},
	}
	manager.SetPluginScheduler(scheduler)

	if _, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil); errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}

	manager.mu.RLock()
	gotAttr := manager.auths["auth-a"].Attributes["team"]
	gotAPIKey := manager.auths["auth-a"].Attributes["api_key"]
	manager.mu.RUnlock()
	if gotAttr != "alpha" {
		t.Fatalf("manager auth attribute team = %q, want alpha", gotAttr)
	}
	if gotAPIKey != "api-key-value" {
		t.Fatalf("manager auth attribute api_key = %q, want api-key-value", gotAPIKey)
	}
}

func TestManagerCustomSelector_FallsBackToLegacyPath(t *testing.T) {
	t.Parallel()

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["auth-a"] = &Auth{ID: "auth-a", Provider: "gemini"}
	manager.auths["auth-b"] = &Auth{ID: "auth-b", Provider: "gemini"}

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if selector.calls != 1 {
		t.Fatalf("selector.calls = %d, want %d", selector.calls, 1)
	}
	if len(selector.lastAuthID) != 2 {
		t.Fatalf("len(selector.lastAuthID) = %d, want %d", len(selector.lastAuthID), 2)
	}
	if got.ID != selector.lastAuthID[len(selector.lastAuthID)-1] {
		t.Fatalf("pickNext() auth.ID = %q, want selector-picked %q", got.ID, selector.lastAuthID[len(selector.lastAuthID)-1])
	}
}

func TestManager_InitializesSchedulerForBuiltInSelector(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	if manager.scheduler == nil {
		t.Fatalf("manager.scheduler = nil")
	}
	if manager.scheduler.strategy != schedulerStrategyRoundRobin {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyRoundRobin)
	}

	manager.SetSelector(&FillFirstSelector{})
	if manager.scheduler.strategy != schedulerStrategyFillFirst {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyFillFirst)
	}
}

func TestManager_SchedulerTracksRegisterAndUpdate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("scheduler.pickSingle() auth = %v, want auth-a", got)
	}

	if _, errUpdate := manager.Update(context.Background(), &Auth{ID: "auth-a", Provider: "gemini", Disabled: true}); errUpdate != nil {
		t.Fatalf("Update(auth-a) error = %v", errUpdate)
	}

	got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after update error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after update auth = %v, want auth-b", got)
	}
}

func TestManager_PickNextMixed_UsesSchedulerRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_SkipsProvidersWithoutExecutors(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "claude-a")
	}
}

func TestManager_SchedulerTracksMarkResultCooldownAndRecovery(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient("auth-b", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient("auth-a")
		reg.UnregisterClient("auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after cooldown auth = %v, want auth-b", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  true,
	})

	seen := make(map[string]struct{}, 2)
	for index := 0; index < 2; index++ {
		got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d auth = nil", index)
		}
		seen[got.ID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("len(seen) = %d, want %d", len(seen), 2)
	}
}

// TestManager_MarkResult_429_NoRetryAfter_TreatedAsTransient verifies that a 429
// without an upstream RetryAfter hint is classified as a transient rate-limit
// (model capacity / TPM burst) instead of a plan-level quota exhaustion: the
// model state's Quota.Exceeded must remain false, and NextRetryAfter must fall
// in a brief cooldown window around now+transientRateLimitCooldown.
func TestManager_MarkResult_429_NoRetryAfter_TreatedAsTransient(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("transient-auth", "gemini", []*registry.ModelInfo{{ID: "transient-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient("transient-auth")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "transient-auth", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(transient-auth) error = %v", errRegister)
	}

	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:   "transient-auth",
		Provider: "gemini",
		Model:    "transient-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit"},
	})
	after := time.Now()

	got, ok := manager.GetByID("transient-auth")
	if !ok || got == nil {
		t.Fatalf("GetByID(transient-auth) = (%v, %v), want auth", got, ok)
	}
	state, exists := got.ModelStates["transient-model"]
	if !exists || state == nil {
		t.Fatalf("ModelStates[transient-model] = (%v, %v), want state", state, exists)
	}
	if state.Quota.Exceeded {
		t.Fatalf("Quota.Exceeded = true, want false (transient 429 must not flip plan-level quota)")
	}
	expectedMin := before.Add(transientRateLimitCooldown).Add(-5 * time.Second)
	expectedMax := after.Add(transientRateLimitCooldown).Add(5 * time.Second)
	if state.NextRetryAfter.Before(expectedMin) || state.NextRetryAfter.After(expectedMax) {
		t.Fatalf("NextRetryAfter = %v, want within [%v, %v]", state.NextRetryAfter, expectedMin, expectedMax)
	}
}

func TestManager_SchedulerSkipsPlanQuotaPlusAndKeepsProAvailable(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	model := "gpt-5.5"
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"codex-plus-a", "codex-plus-b", "codex-pro"} {
		reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, id := range []string{"codex-plus-a", "codex-plus-b", "codex-pro"} {
			reg.UnregisterClient(id)
		}
	})
	for _, auth := range []*Auth{
		{ID: "codex-plus-a", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: "codex-plus-b", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: "codex-pro", Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	retryAfter := 2 * time.Hour
	for _, id := range []string{"codex-plus-a", "codex-plus-b"} {
		manager.MarkResult(context.Background(), Result{
			AuthID:     id,
			Provider:   "codex",
			Model:      model,
			Success:    false,
			Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
			RetryAfter: &retryAfter,
		})
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "codex-pro" {
		t.Fatalf("scheduler.pickSingle() auth = %v, want codex-pro", got)
	}
}

func TestManager_StreamChunkUsageLimitMarksPlanQuota(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	auth := &Auth{ID: "codex-plus-stream", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	retryAfter := 2 * time.Hour
	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Err: retryAfterStatusErr{
		code:       http.StatusTooManyRequests,
		msg:        "You've hit your usage limit. Upgrade to Pro or try again later.",
		retryAfter: retryAfter,
	}}
	close(remaining)

	result := manager.wrapStreamResult(
		context.Background(),
		auth.Clone(),
		"codex",
		"gpt-5.5",
		nil,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("data: first\n\n")}},
		remaining,
		OAuthModelAliasResult{},
		false,
	)
	for range result.Chunks {
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID() = (%v, %v), want auth", got, ok)
	}
	state := got.ModelStates["gpt-5.5"]
	if state == nil {
		t.Fatal("ModelStates[gpt-5.5] missing")
	}
	if !state.Quota.Exceeded {
		t.Fatal("Quota.Exceeded = false, want true for usage-limit stream error")
	}
	if state.Quota.Reason != "quota" {
		t.Fatalf("Quota.Reason = %q, want quota", state.Quota.Reason)
	}
	if state.NextRetryAfter.Before(time.Now().Add(retryAfter - time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want roughly now + %v", state.NextRetryAfter, retryAfter)
	}
}

func TestManager_StreamSuccessCodexExhaustedHeaderCoolsAuth(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	model := "gpt-5.5"
	plusID := "codex-plus-header-exhausted"
	proID := "codex-pro-header-available"
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{plusID, proID} {
		reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, id := range []string{plusID, proID} {
			reg.UnregisterClient(id)
		}
	})
	plusAuth := &Auth{ID: plusID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}
	proAuth := &Auth{ID: proID, Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}}
	for _, auth := range []*Auth{plusAuth, proAuth} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	resetAt := time.Now().Add(2 * time.Hour)
	headers := exhaustedCodexHeaders(resetAt)
	remaining := make(chan cliproxyexecutor.StreamChunk)
	close(remaining)

	result := manager.wrapStreamResult(
		context.Background(),
		plusAuth.Clone(),
		"codex",
		model,
		headers,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("data: done\n\n")}},
		remaining,
		OAuthModelAliasResult{},
		false,
	)
	for range result.Chunks {
	}

	gotPlus, ok := manager.GetByID(plusID)
	if !ok || gotPlus == nil {
		t.Fatalf("GetByID(%s) = (%v, %v), want auth", plusID, gotPlus, ok)
	}
	state := gotPlus.ModelStates[model]
	if state == nil {
		t.Fatalf("ModelStates[%s] missing", model)
	}
	if !state.Quota.Exceeded {
		t.Fatal("Quota.Exceeded = false, want true from exhausted Codex success headers")
	}
	if state.NextRetryAfter.Before(resetAt.Add(-time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want near reset %v", state.NextRetryAfter, resetAt)
	}

	picked, errPick := manager.scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != proID {
		t.Fatalf("scheduler.pickSingle() auth = %v, want %s", picked, proID)
	}
}

func TestManager_ExecuteSuccessCodexExhaustedHeaderCoolsAuth(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	model := "gpt-5.5"
	plusID := "codex-plus-execute-header-exhausted"
	proID := "codex-pro-execute-header-available"
	registerSchedulerModels(t, "codex", model, plusID, proID)
	manager.executors["codex"] = quotaHeaderExecutor{headers: exhaustedCodexHeaders(time.Now().Add(2 * time.Hour))}
	for _, auth := range []*Auth{
		{ID: plusID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: proID, Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	metadata := map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: plusID}
	if _, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: metadata}); errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}

	gotPlus, ok := manager.GetByID(plusID)
	if !ok || gotPlus == nil {
		t.Fatalf("GetByID(%s) missing", plusID)
	}
	state := gotPlus.ModelStates[model]
	if state == nil || !state.Quota.Exceeded {
		t.Fatalf("ModelStates[%s].Quota.Exceeded = %#v, want true", model, state)
	}

	picked, errPick := manager.scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != proID {
		t.Fatalf("scheduler.pickSingle() auth = %v, want %s", picked, proID)
	}
}

func TestManager_ExecuteCountSuccessCodexExhaustedHeaderCoolsAuth(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	model := "gpt-5.5"
	plusID := "codex-plus-count-header-exhausted"
	proID := "codex-pro-count-header-available"
	registerSchedulerModels(t, "codex", model, plusID, proID)
	manager.executors["codex"] = quotaHeaderExecutor{headers: exhaustedCodexHeaders(time.Now().Add(2 * time.Hour))}
	for _, auth := range []*Auth{
		{ID: plusID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: proID, Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	metadata := map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: plusID}
	if _, errExec := manager.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Metadata: metadata}); errExec != nil {
		t.Fatalf("ExecuteCount() error = %v", errExec)
	}

	gotPlus, ok := manager.GetByID(plusID)
	if !ok || gotPlus == nil {
		t.Fatalf("GetByID(%s) missing", plusID)
	}
	state := gotPlus.ModelStates[model]
	if state == nil || !state.Quota.Exceeded {
		t.Fatalf("ModelStates[%s].Quota.Exceeded = %#v, want true", model, state)
	}

	picked, errPick := manager.scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != proID {
		t.Fatalf("scheduler.pickSingle() auth = %v, want %s", picked, proID)
	}
}

// TestManager_AfterTransient429AllAuths_RecoversWithinShortWindow simulates the
// production bug we just patched: a single client request fans out across N
// auths under the same provider/model and every upstream call returns 429 with
// no RetryAfter (the canonical transient signature).
//
// Pre-fix: each auth would be classified as plan-quota, Quota.Exceeded would
// flip to true, and once all N were tagged, the selector raised
// *modelCooldownError, surfacing 429 model_cooldown to the user even though
// upstream had only exhibited a brief TPM/capacity burst.
//
// Post-fix: every state must keep Quota.Exceeded == false (no plan-quota
// false-positive), and the pool must be merely "auth_unavailable" for a short
// window (~transientRateLimitCooldown == 1 minute), recovering as soon as the
// per-state NextRetryAfter elapses.
func TestManager_AfterTransient429AllAuths_RecoversWithinShortWindow(t *testing.T) {
	t.Parallel()

	provider := "gemini"
	model := "transient-pool-model"
	authIDs := []string{"transient-pool-a", "transient-pool-b", "transient-pool-c"}

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	registerSchedulerModels(t, provider, model, authIDs...)
	for _, id := range authIDs {
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: provider}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	// Fan-out 429 with nil RetryAfter on every auth (mimics conductor's
	// inner per-auth retry loop hitting transient rate-limits across the pool).
	for _, id := range authIDs {
		manager.MarkResult(context.Background(), Result{
			AuthID:   id,
			Provider: provider,
			Model:    model,
			Success:  false,
			Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit"},
		})
	}

	// (a) plan-quota must NOT be flipped on any auth.
	for _, id := range authIDs {
		got, ok := manager.GetByID(id)
		if !ok || got == nil {
			t.Fatalf("GetByID(%s) = (%v, %v), want auth", id, got, ok)
		}
		state, exists := got.ModelStates[model]
		if !exists || state == nil {
			t.Fatalf("%s: ModelStates[%s] missing, want state", id, model)
		}
		if state.Quota.Exceeded {
			t.Fatalf("%s: Quota.Exceeded = true, want false (transient must not flip plan-quota)", id)
		}
		if state.Quota.Reason != "transient" {
			t.Fatalf("%s: Quota.Reason = %q, want %q", id, state.Quota.Reason, "transient")
		}
	}

	// (b) within the cooldown window, the entire pool is unavailable but NOT
	// classified as model_cooldown (because Quota.Exceeded is false on every
	// state, so the cooldownCount path in getAvailableAuths is not triggered).
	got, errPick := manager.scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if got != nil {
		t.Fatalf("pickSingle() during transient window auth = %v, want nil", got)
	}
	var cooldownErr *modelCooldownError
	if errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() during transient window error = %T (model_cooldown), want non-cooldown auth_unavailable", errPick)
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) {
		t.Fatalf("pickSingle() during transient window error = %v, want *Error", errPick)
	}
	if authErr.Code != "auth_unavailable" {
		t.Fatalf("pickSingle() during transient window error.Code = %q, want %q", authErr.Code, "auth_unavailable")
	}

	// (c) simulate the transient cooldown elapsing by rewinding each state's
	// NextRetryAfter into the past. We mutate the manager's internal auth
	// pointers (same package access) and re-publish the snapshots into the
	// scheduler so the scheduledAuth's cached nextRetryAt is also rewound;
	// this mirrors how the conductor itself propagates state updates without
	// sleeping in tests.
	pastTime := time.Now().Add(-1 * time.Second)
	manager.mu.Lock()
	for _, id := range authIDs {
		auth, ok := manager.auths[id]
		if !ok || auth == nil {
			manager.mu.Unlock()
			t.Fatalf("manager.auths[%s] missing, want present", id)
		}
		state := auth.ModelStates[model]
		state.NextRetryAfter = pastTime
		state.Quota.NextRecoverAt = pastTime
		auth.NextRetryAfter = pastTime
		auth.Quota.NextRecoverAt = pastTime
	}
	snapshots := make([]*Auth, 0, len(authIDs))
	for _, id := range authIDs {
		snapshots = append(snapshots, manager.auths[id].Clone())
	}
	manager.mu.Unlock()
	for _, snapshot := range snapshots {
		manager.scheduler.upsertAuth(snapshot)
	}

	// After the window, the pool recovers without manual intervention.
	got, errPick = manager.scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() after transient window error = %v, want nil", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() after transient window auth = nil, want one of %v", authIDs)
	}
	matched := false
	for _, id := range authIDs {
		if got.ID == id {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("pickSingle() after transient window auth.ID = %q, want one of %v", got.ID, authIDs)
	}
}
