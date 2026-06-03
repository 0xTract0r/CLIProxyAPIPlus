package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// TestCodexExecutor_Transient429AcrossPool_NoPlanQuotaFalsePositive_RecoversAfterWindow
// 是 cliproxy 429 分类补丁的端到端覆盖：
//
//   - codex executor 的 parseCodexRetryAfter 仅在 upstream payload 出现
//     `error.type == "usage_limit_reached"` 时把 RetryAfter 设为非 nil；
//   - 其他 429（model_capacity / TPM 抖动 / 并发 burst）会走 transient 分支，
//     保持 Quota.Exceeded == false、Reason == "transient"，pool 仅短窗口
//     auth_unavailable，绝不上升为 model_cooldown；
//   - cooldown 窗口结束后，pool 应自动恢复并能成功完成请求。
//
// 不打真上游：所有 auth 的 base_url 都指向 httptest.NewServer 起的 stub，
// 用 atomic.Bool 控制阶段（429 → 200）。也不睡：阶段 (3) 的"窗口过去"
// 通过 Manager.Update 把 ModelStates 的 NextRetryAfter 倒拨到过去并触发
// scheduler.upsertAuth，复用与 conductor 自身一致的 state-propagation 路径。
func TestCodexExecutor_Transient429AcrossPool_NoPlanQuotaFalsePositive_RecoversAfterWindow(t *testing.T) {
	provider := "codex"
	model := "gpt-5.4-mini"
	authIDs := []string{"codex-pool-a", "codex-pool-b", "codex-pool-c"}

	// stub 上游：阶段 1 永远返回 429（无 usage_limit_reached、无 Retry-After），
	// 阶段 2 返回最简 SSE 200 让 codex executor 走成功分支。
	var serveSuccess atomic.Bool
	var hits429, hits200 atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSuccess.Load() {
			hits200.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			// 复用 codex_executor_stream_output_test.go 已经验证过的最简 SSE 形态：
			// response.output_item.done + response.completed，让 Execute 走成功路径。
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"" + model + "\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
			return
		}
		hits429.Add(1)
		// 故意不带 Retry-After header，body 也不写 usage_limit_reached：
		// parseCodexRetryAfter 会因此返回 nil → conductor 走 transient 分支。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"transient stub: TPM burst, please retry shortly"}}`))
	}))
	defer server.Close()

	// 注册 manager + codex executor，所有 auth 都指向 stub。
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))

	reg := registry.GetGlobalRegistry()
	for _, id := range authIDs {
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, id := range authIDs {
			reg.UnregisterClient(id)
		}
	})

	for _, id := range authIDs {
		auth := &cliproxyauth.Auth{
			ID:       id,
			Provider: provider,
			Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "stub-test-key-" + id,
			},
		}
		if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
			t.Fatalf("Register(%s) error = %v", id, errReg)
		}
		// Register 在 registry.RegisterClient 之前完成 scheduler.upsertAuth，
		// 此时 supportedModelSet 还是空；显式刷新让 model 路由生效。
		manager.RefreshSchedulerEntry(id)
	}

	// 阶段 (1)：用户单条请求经 Manager.Execute 触发 fan-out。
	// 默认 retrySettings 全为 0，shouldRetryAfterError 直接返回 false，
	// 内层 executeMixedOnce 跑完 3 条 auth 后回错给上层。
	_, errExec := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{
			Model:   model,
			Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Say ok"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Stream:       false,
		},
	)
	if errExec == nil {
		t.Fatalf("Execute() phase 1 error = nil, want non-nil (every stub upstream returns 429)")
	}
	if got := hits429.Load(); got < int32(len(authIDs)) {
		t.Fatalf("phase 1 stub 429 hits = %d, want >= %d (one per auth)", got, len(authIDs))
	}
	if got := hits200.Load(); got != 0 {
		t.Fatalf("phase 1 stub 200 hits = %d, want 0", got)
	}

	// 断言 (a)：每条 auth 的 ModelState 都不应被错判为 plan-quota。
	for _, id := range authIDs {
		got, ok := manager.GetByID(id)
		if !ok || got == nil {
			t.Fatalf("GetByID(%s) = (%v, %v), want auth", id, got, ok)
		}
		state, exists := got.ModelStates[model]
		if !exists || state == nil {
			t.Fatalf("%s: ModelStates[%s] missing, want state populated by transient 429", id, model)
		}
		if state.Quota.Exceeded {
			t.Fatalf("%s: ModelStates[%s].Quota.Exceeded = true, want false (transient 429 must not flip plan-quota)", id, model)
		}
		if state.Quota.Reason != "transient" {
			t.Fatalf("%s: ModelStates[%s].Quota.Reason = %q, want %q", id, model, state.Quota.Reason, "transient")
		}
		// auth 顶层 Quota 也不应被翻成 plan-quota（applyAuthFailureState 只在
		// 没有 model 信息时才被调用；conductor 当前路径会进 per-model 分支）。
		if got.Quota.Exceeded {
			t.Fatalf("%s: top-level Quota.Exceeded = true, want false", id)
		}
	}

	// 阶段 (2)：短窗口内 scheduler.pickSingle 应是 auth_unavailable，绝不是
	// model_cooldown（否则用户会看到 429 model_cooldown 假阳性）。
	// pickSingle 是 scheduler 的内部 helper，executor 包没有直接句柄，
	// 但 Manager.Execute 在窗口未过时同样不应给出可用 auth：再发一次请求，
	// 期望仍然失败且 stub 不被命中。
	hits429Before := hits429.Load()
	hits200Before := hits200.Load()
	_, errPhase2 := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{
			Model:   model,
			Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Say ok"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Stream:       false,
		},
	)
	if errPhase2 == nil {
		t.Fatalf("Execute() phase 2 error = nil, want non-nil (cooldown window not yet elapsed)")
	}
	if got := hits429.Load() - hits429Before; got != 0 {
		t.Fatalf("phase 2 stub additional 429 hits = %d, want 0 (pool must be locally unavailable, no upstream call)", got)
	}
	if got := hits200.Load() - hits200Before; got != 0 {
		t.Fatalf("phase 2 stub additional 200 hits = %d, want 0", got)
	}
	// errPhase2 必须可解析成 *cliproxyauth.Error，且 Code 与 modelCooldownError
	// 区分（模型级 cooldown 会带 model_cooldown code 或 modelCooldownError 类型）。
	var phase2Err *cliproxyauth.Error
	if !errors.As(errPhase2, &phase2Err) {
		t.Fatalf("phase 2 error type = %T (%v), want *cliproxyauth.Error", errPhase2, errPhase2)
	}
	if phase2Err.Code == "model_cooldown" {
		t.Fatalf("phase 2 error.Code = %q, want non model_cooldown (transient 429 must not surface as model_cooldown)", phase2Err.Code)
	}
	// 此时仍然属于"短窗口 auth 不可用"，conductor 在所有 auth 都 tried 后会
	// 返回 auth_not_found 或 auth_unavailable；只要不是 model_cooldown 就 OK。
	if phase2Err.Code != "auth_not_found" && phase2Err.Code != "auth_unavailable" {
		t.Fatalf("phase 2 error.Code = %q, want %q or %q", phase2Err.Code, "auth_unavailable", "auth_not_found")
	}

	// 阶段 (3)：把每条 auth 的 ModelState.NextRetryAfter 倒拨到过去，并通过
	// Manager.Update 触发 scheduler.upsertAuth，模拟 transient cooldown 走完。
	pastTime := time.Now().Add(-1 * time.Second)
	for _, id := range authIDs {
		current, ok := manager.GetByID(id)
		if !ok || current == nil {
			t.Fatalf("GetByID(%s) before rewind = (%v, %v)", id, current, ok)
		}
		state, exists := current.ModelStates[model]
		if !exists || state == nil {
			t.Fatalf("%s: ModelStates[%s] missing before rewind", id, model)
		}
		state.NextRetryAfter = pastTime
		state.Quota.NextRecoverAt = pastTime
		state.Unavailable = false
		current.NextRetryAfter = pastTime
		current.Quota.NextRecoverAt = pastTime
		current.Unavailable = false
		current.Status = cliproxyauth.StatusActive
		if _, errUpd := manager.Update(context.Background(), current); errUpd != nil {
			t.Fatalf("Update(%s) after rewind error = %v", id, errUpd)
		}
	}

	// 把 stub 切换到 200 阶段。
	serveSuccess.Store(true)
	hits200Before = hits200.Load()

	resp, errPhase3 := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{
			Model:   model,
			Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Say ok"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Stream:       false,
		},
	)
	if errPhase3 != nil {
		t.Fatalf("Execute() phase 3 error = %v, want nil (cooldown elapsed, stub now returns 200)", errPhase3)
	}
	if got := hits200.Load() - hits200Before; got < 1 {
		t.Fatalf("phase 3 stub 200 hits delta = %d, want >= 1", got)
	}
	if gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotContent != "ok" {
		t.Fatalf("phase 3 choices.0.message.content = %q, want %q; payload=%s", gotContent, "ok", string(resp.Payload))
	}
}
