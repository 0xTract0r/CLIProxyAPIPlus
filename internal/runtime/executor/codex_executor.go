package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func captureManagedHeaderSnapshot(headers http.Header, names []string) map[string]string {
	if headers == nil || len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func applyManagedHeaderSnapshot(headers http.Header, snapshot map[string]string) {
	if headers == nil || len(snapshot) == 0 {
		return
	}
	for name, value := range snapshot {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		headers.Set(name, value)
	}
}

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
//
// 当通过 NewCodexExecutorWithManager 构造时，executor 会在检测到 Codex 上游
// cyber_policy 事件时同步更新对应 Auth 的 CyberPolicyFlagCount / LastCyberPolicyAt。
// 旧的 NewCodexExecutor 入口保持 nil manager，仅用于不需要计数写回的测试场景。
type CodexExecutor struct {
	cfg         *config.Config
	authManager *cliproxyauth.Manager
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

// NewCodexExecutorWithManager wires the auth manager so cyber_policy hits are
// persisted into the auth record (CyberPolicyFlagCount / LastCyberPolicyAt).
func NewCodexExecutorWithManager(cfg *config.Config, manager *cliproxyauth.Manager) *CodexExecutor {
	return &CodexExecutor{cfg: cfg, authManager: manager}
}

func (e *CodexExecutor) Identifier() string { return "codex" }

// persistCodexDeviceHighWater resolves the account's current outbound codex
// client profile (the same resolution applyCodexHeaders / applyCodexWebsocketHeaders
// just performed) and asks the auth manager to monotonically raise the persisted
// high-water (auth.Metadata[codex_device_high_water]).
//
// It is a no-op when no manager is wired, when the auth has no ID, or when no
// usable CLI version could be resolved. The manager performs the strict-increase
// comparison, so calling this every request is safe and writes to disk only on an
// actual raise.
//
// 与 claude 的 persistClaudeDeviceHighWater 对称。挂点必须在真实 serving 路径上
// （Execute/executeCompact/ExecuteStream + WS 出站），紧挨 applyCodexHeaders 之后，
// 不挂 PrepareRequest——codex PrepareRequest 只服务 HttpRequest adapter 旁路、不解析
// 客户端画像，挂在那里写回永不触发，重启回落 floor。ctx 携带 gin headers，
// CodexObservedHighWaterForAuth 复用同一组入站 header 解析观测版本。
func (e *CodexExecutor) persistCodexDeviceHighWater(ctx context.Context, auth *cliproxyauth.Auth) {
	if e == nil || e.authManager == nil || auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	var ginHeaders http.Header
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	highWater, ok := helps.CodexObservedHighWaterForAuth(auth, ginHeaders, e.cfg)
	if !ok {
		return
	}
	if _, err := e.authManager.RaiseCodexDeviceHighWater(ctx, authID, highWater); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"component": "codex_device_high_water",
			"auth_id":   authID,
		}).Warn("codex executor: failed to persist device-profile high-water")
	}
}

func refreshFailureLogFields(auth *cliproxyauth.Auth) log.Fields {
	fields := log.Fields{}
	if auth == nil {
		return fields
	}
	fields["provider"] = auth.Provider
	if remark := authAccountRemark(auth); remark != "" {
		fields["account_remark"] = remark
	}
	// Keep stable identifiers in structured data for local forensic correlation;
	// the Feishu error hook redacts these fields before delivery.
	fields["auth_id"] = auth.ID
	fields["auth_file"] = auth.FileName
	return fields
}

func authAccountRemark(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if note := strings.TrimSpace(auth.Attributes["note"]); note != "" {
			return note
		}
	}
	if auth.Metadata != nil {
		if note, ok := auth.Metadata["note"].(string); ok {
			if trimmed := strings.TrimSpace(note); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
