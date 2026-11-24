package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ordinaryHTTPAdmissionExecutor struct {
	coreauth.ProviderExecutor
	selected string
	calls    atomic.Int32
	stream   chan exec.StreamChunk
	started  chan struct{}
	refuse   bool
}

func (*ordinaryHTTPAdmissionExecutor) Identifier() string { return "codex" }
func (e *ordinaryHTTPAdmissionExecutor) observe(a *coreauth.Auth) error {
	e.calls.Add(1)
	if a == nil || a.ID != e.selected {
		return fmt.Errorf("unexpected grant")
	}
	return nil
}

func (e *ordinaryHTTPAdmissionExecutor) Execute(_ context.Context, a *coreauth.Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
	if err := e.observe(a); err != nil {
		return exec.Response{}, err
	}
	if e.refuse {
		return exec.Response{}, ordinaryHTTPAdmissionRefusal{}
	}
	return exec.Response{Payload: []byte(`{"id":"independent","object":"response","status":"completed","output":[]}`)}, nil
}

func (e *ordinaryHTTPAdmissionExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
	if err := e.observe(a); err != nil {
		return nil, err
	}
	close(e.started)
	return &exec.StreamResult{Chunks: e.stream}, nil
}

type ordinaryHTTPAdmissionRefusal struct{}

func (ordinaryHTTPAdmissionRefusal) Error() string   { return "fixture provider refused" }
func (ordinaryHTTPAdmissionRefusal) StatusCode() int { return http.StatusTooManyRequests }
func (ordinaryHTTPAdmissionRefusal) RetryAfter() *time.Duration {
	delay := 75 * time.Second
	return &delay
}

func newOrdinaryHTTPAdmission(t *testing.T, e *ordinaryHTTPAdmissionExecutor) *gin.Engine {
	t.Helper()
	cfg, err := config.ParseConfigBytes([]byte("routing:\n  strategy: fill-first\n  max-concurrent-per-credential: 1\nrequest-retry: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.AuthDir = dir
	s, err := NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(dir, "config.yaml")).Build()
	if err != nil {
		t.Fatal(err)
	}
	s.coreManager.RegisterExecutor(e)
	if _, err := s.coreManager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: e.selected, Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	r := registry.GetGlobalRegistry()
	r.RegisterClient(e.selected, "codex", []*registry.ModelInfo{{ID: "gpt-5-codex"}})
	t.Cleanup(func() { r.UnregisterClient(e.selected) })
	h := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, s.coreManager))
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	return router
}

func ordinaryAdmissionPOST(ctx context.Context, router http.Handler, stream bool) *httptest.ResponseRecorder {
	payload := fmt.Sprintf(`{"model":"gpt-5-codex","stream":%t,"input":"hello"}`, stream)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	return w
}

func TestOrdinaryServiceHTTPAdmissionCancelsWaitingAndReleasesStream(t *testing.T) {
	e := &ordinaryHTTPAdmissionExecutor{selected: t.Name()}
	router := newOrdinaryHTTPAdmission(t, e)
	synctest.Test(t, func(t *testing.T) {
		e.started = make(chan struct{})
		e.stream = make(chan exec.StreamChunk, 1)
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- ordinaryAdmissionPOST(t.Context(), router, true) }()
		synctest.Wait()
		select {
		case <-e.started:
		default:
			t.Fatal("first request did not start streaming")
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waiter := make(chan *httptest.ResponseRecorder, 1)
		go func() { waiter <- ordinaryAdmissionPOST(ctx, router, false) }()
		synctest.Wait()
		select {
		case <-waiter:
			t.Fatal("request returned before slot released")
		default:
		}
		if got := e.calls.Load(); got != 1 {
			t.Fatalf("queued request reached executor: %d", got)
		}
		cancel()
		synctest.Wait()
		select {
		case <-waiter:
		default:
			t.Fatal("queue cancellation did not finish")
		}
		if got := e.calls.Load(); got != 1 {
			t.Fatalf("canceled queue request reached executor: %d", got)
		}
		e.stream <- exec.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")}
		close(e.stream)
		synctest.Wait()
		select {
		case res := <-first:
			if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "response.completed") {
				t.Fatalf("stream completion status=%d body=%q", res.Code, res.Body.String())
			}
		default:
			t.Fatal("original stream did not finish")
		}
		if res := ordinaryAdmissionPOST(t.Context(), router, false); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "independent") || e.calls.Load() != 2 {
			t.Fatalf("independent request: status=%d calls=%d body=%q", res.Code, e.calls.Load(), res.Body.String())
		}
	})
}

func TestOrdinaryServiceHTTPAdmissionPreservesRetryAfter(t *testing.T) {
	e := &ordinaryHTTPAdmissionExecutor{selected: t.Name(), refuse: true}
	router := newOrdinaryHTTPAdmission(t, e)
	res := ordinaryAdmissionPOST(t.Context(), router, false)
	if res.Code != http.StatusTooManyRequests || res.Header().Get("Retry-After") != "75" ||
		!strings.Contains(res.Body.String(), "fixture provider refused") || e.calls.Load() != 1 {
		t.Fatalf("refusal: status=%d retry-after=%q calls=%d body=%q", res.Code, res.Header().Get("Retry-After"), e.calls.Load(), res.Body.String())
	}
}
