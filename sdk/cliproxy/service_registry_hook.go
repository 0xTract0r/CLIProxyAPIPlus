package cliproxy

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// authRegistryHook keeps the global model registry and scheduler in sync with
// core-manager auth lifecycle events. On every add/update it re-runs the
// plan-filtered model registration so subscription-tier gating (Opus/Codex) is
// re-evaluated when an auth's plan attributes change.
type authRegistryHook struct {
	service *Service
}

func (h authRegistryHook) OnAuthRegistered(ctx context.Context, auth *coreauth.Auth) {
	h.refresh(ctx, auth)
}

func (h authRegistryHook) OnAuthUpdated(ctx context.Context, auth *coreauth.Auth) {
	h.refresh(ctx, auth)
}

func (h authRegistryHook) OnResult(context.Context, coreauth.Result) {}

func (h authRegistryHook) refresh(ctx context.Context, auth *coreauth.Auth) {
	if h.service == nil || h.service.coreManager == nil || auth == nil {
		return
	}
	h.service.registerModelsForAuth(ctx, auth)
	h.service.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)
	h.service.coreManager.RefreshSchedulerEntry(auth.ID)
}
