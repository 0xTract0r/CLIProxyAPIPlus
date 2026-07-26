package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// fork(anticorr) F6 guard — gemini-cli executor registration.
//
// The gemini-cli executor is what carries the fork's anti-correlation outbound
// behavior for gemini-cli auths. It is registered in two independent places in
// service.go:
//
//   - the per-auth bind path ensureExecutorsForAuthWithMode: `case "gemini-cli"`
//     (service.go ~L466-467), and
//   - the home/baseline path registerHomeExecutors:
//     `RegisterExecutor(NewGeminiCLIExecutor(...))` (service.go ~L651).
//
// Before this guard there was zero test coverage asserting either registration,
// so an upstream merge that drops either line would still compile green and the
// provider would silently lose its executor (dispatch falls back to the generic
// openai-compat path or fails to resolve), quietly discarding the anti-correlation
// wiring. These tests fail (red) if either registration line is removed.

// TestEnsureExecutorsForAuth_GeminiCLIBindsExecutor guards the per-auth switch
// case. Red condition: delete `case "gemini-cli"` / its RegisterExecutor line in
// ensureExecutorsForAuthWithMode (service.go ~L466-467) — the switch then falls
// through to the default openai-compat branch and Executor("gemini-cli") returns
// a non-GeminiCLIExecutor (or the type assertion below fails).
func TestEnsureExecutorsForAuth_GeminiCLIBindsExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "gemini-cli-auth-1",
		Provider: "gemini-cli",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	resolved, ok := service.coreManager.Executor("gemini-cli")
	if !ok || resolved == nil {
		t.Fatal("expected gemini-cli executor after per-auth bind, got none")
	}
	if got := resolved.Identifier(); got != "gemini-cli" {
		t.Fatalf("resolved executor Identifier() = %q, want \"gemini-cli\"", got)
	}
	if _, isGeminiCLI := resolved.(*executor.GeminiCLIExecutor); !isGeminiCLI {
		t.Fatalf("executor type = %T, want *executor.GeminiCLIExecutor (per-auth bind must register the gemini-cli executor, not a fallback)", resolved)
	}
}

// TestRegisterHomeExecutors_IncludesGeminiCLI guards the home/baseline
// registration. Red condition: delete the
// `RegisterExecutor(NewGeminiCLIExecutor(...))` line in registerHomeExecutors
// (service.go ~L651) — home-dispatched gemini-cli auths then have no executor
// and Executor("gemini-cli") returns ok=false.
func TestRegisterHomeExecutors_IncludesGeminiCLI(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	service.registerHomeExecutors()

	resolved, ok := service.coreManager.Executor("gemini-cli")
	if !ok || resolved == nil {
		t.Fatal("expected gemini-cli executor after registerHomeExecutors, got none")
	}
	if got := resolved.Identifier(); got != "gemini-cli" {
		t.Fatalf("resolved executor Identifier() = %q, want \"gemini-cli\"", got)
	}
	if _, isGeminiCLI := resolved.(*executor.GeminiCLIExecutor); !isGeminiCLI {
		t.Fatalf("executor type = %T, want *executor.GeminiCLIExecutor (home baseline must register the gemini-cli executor)", resolved)
	}
}
