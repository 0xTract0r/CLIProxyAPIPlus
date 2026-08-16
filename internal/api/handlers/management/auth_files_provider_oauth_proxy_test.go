package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// 回归守护：core 内建 provider 的 OAuth 加号 handler 必须把账号住宅代理
//   (1) 绑进 OAuth token 交换的出站 HTTP（走账号代理、不回退核心全局代理），
//   (2) 写进落库 record 的 proxy_url。
// 上游同步 c8132e1d 覆盖 auth_files_provider_oauth.go 后曾把这两条接线一起丢掉，
// 导致新账号 proxy_url 为空。下面按 handler 实际使用的接线原语逐 provider 断言。

const (
	oauthAccountProxy = "socks5://account.example:1080"
	oauthGlobalProxy  = "socks5://global.example:9999"
)

func newProxyWiringTestHandler(t *testing.T) *Handler {
	t.Helper()
	// 全局代理刻意设成另一台，用来证明 token 交换绑的是账号代理而不是全局代理。
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.SDKConfig.ProxyURL = oauthGlobalProxy
	return NewHandlerWithoutConfigFilePath(cfg, coreauth.NewManager(nil, nil, nil))
}

// TestRequestAnthropicTokenWiresAccountProxyIntoExchangeAndRecord 逐字覆盖 claude：
// 交换侧走 prepareOAuthSetupRuntimeAuth("claude", setup) 合成的账号身份（喂给
// newClaudeOAuthAuth → oauthIdentityHTTPClient，代理取账号代理），落库侧把 proxy 写进 record。
func TestRequestAnthropicTokenWiresAccountProxyIntoExchangeAndRecord(t *testing.T) {
	h := newProxyWiringTestHandler(t)
	setup := &oauthAccountSetup{ProxyURL: oauthAccountProxy}

	// 交换侧：claude handler 的 exchangeAuth。
	exchangeAuth := h.prepareOAuthSetupRuntimeAuth("claude", setup)
	if exchangeAuth == nil {
		t.Fatal("claude exchange auth missing; token exchange would fall back to the global core proxy")
	}
	if exchangeAuth.Provider != "claude" {
		t.Fatalf("claude exchange auth provider = %q, want claude", exchangeAuth.Provider)
	}
	if exchangeAuth.ProxyURL != oauthAccountProxy {
		t.Fatalf("claude exchange ProxyURL = %q, want account proxy %q", exchangeAuth.ProxyURL, oauthAccountProxy)
	}
	if exchangeAuth.ProxyURL == oauthGlobalProxy {
		t.Fatal("claude exchange bound the global core proxy instead of the account proxy")
	}

	// 落库侧：claude handler 的 record（与真实 handler 同形状）。
	record := &coreauth.Auth{
		ID:       "claude-new@example.test.json",
		FileName: "claude-new@example.test.json",
		Provider: "claude",
		Metadata: map[string]any{"email": "new@example.test"},
	}
	copyOAuthSetupSeed(record, exchangeAuth)
	h.applyOAuthAccountSetupToRecord(record, setup)

	if record.ProxyURL != oauthAccountProxy {
		t.Fatalf("claude record.ProxyURL = %q, want %q", record.ProxyURL, oauthAccountProxy)
	}
	if got, _ := record.Metadata["proxy_url"].(string); got != oauthAccountProxy {
		t.Fatalf("claude record.Metadata[proxy_url] = %q, want %q", got, oauthAccountProxy)
	}
}

// TestRequestBuiltinProviderTokenWiresAccountProxyIntoExchangeAndRecord 表驱动覆盖其余 4 个内建
// provider。codex 与 claude 同路（oauthIdentityHTTPClient，判据=prepareOAuthSetupRuntimeAuth
// 合成的账号身份 ProxyURL）；antigravity / xai / kimi 走 configForOAuthSetup（判据=effective
// config 的 SDKConfig.ProxyURL 取账号代理、全局 cfg 不被污染）。全部再断言 proxy 写进落库 record。
func TestRequestBuiltinProviderTokenWiresAccountProxyIntoExchangeAndRecord(t *testing.T) {
	const (
		exchangeViaRuntimeAuth = "runtime_auth" // codex：oauthIdentityHTTPClient(账号身份)
		exchangeViaConfig      = "config"       // antigravity/xai/kimi：NewXxxAuth(configForOAuthSetup(setup))
	)

	cases := []struct {
		name        string
		provider    string // 传给 prepareOAuthSetupRuntimeAuth 的 provider 字符串
		exchangeVia string
		record      *coreauth.Auth
	}{
		{
			name:        "codex",
			provider:    "codex",
			exchangeVia: exchangeViaRuntimeAuth,
			record: &coreauth.Auth{
				ID: "codex-new.json", FileName: "codex-new.json", Provider: "codex",
				Metadata: map[string]any{"email": "new@example.test", "account_id": "acc-1"},
			},
		},
		{
			name:        "antigravity",
			provider:    "antigravity",
			exchangeVia: exchangeViaConfig,
			record: &coreauth.Auth{
				ID: "antigravity-new.json", FileName: "antigravity-new.json", Provider: "antigravity",
				Metadata: map[string]any{"type": "antigravity", "email": "new@example.test"},
			},
		},
		{
			name:        "xai",
			provider:    "xai",
			exchangeVia: exchangeViaConfig,
			record: &coreauth.Auth{
				ID: "xai-new.json", FileName: "xai-new.json", Provider: "xai",
				Metadata: map[string]any{"type": "xai", "email": "new@example.test"},
			},
		},
		{
			name:        "kimi",
			provider:    "kimi",
			exchangeVia: exchangeViaConfig,
			record: &coreauth.Auth{
				ID: "kimi-new.json", FileName: "kimi-new.json", Provider: "kimi",
				Metadata: map[string]any{"type": "kimi"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProxyWiringTestHandler(t)
			setup := &oauthAccountSetup{ProxyURL: oauthAccountProxy}

			switch tc.exchangeVia {
			case exchangeViaRuntimeAuth:
				// token 交换的出站 HTTP 由账号身份（喂给 oauthIdentityHTTPClient）承载，
				// 代理取账号代理而非全局。
				exchangeAuth := h.prepareOAuthSetupRuntimeAuth(tc.provider, setup)
				if exchangeAuth == nil {
					t.Fatalf("%s exchange auth missing; token exchange would fall back to the global core proxy", tc.provider)
				}
				if exchangeAuth.ProxyURL != oauthAccountProxy {
					t.Fatalf("%s exchange ProxyURL = %q, want account proxy %q", tc.provider, exchangeAuth.ProxyURL, oauthAccountProxy)
				}
				if exchangeAuth.ProxyURL == oauthGlobalProxy {
					t.Fatalf("%s exchange bound the global core proxy instead of the account proxy", tc.provider)
				}
			case exchangeViaConfig:
				// token 交换的 HTTP client 由 auth 服务从 configForOAuthSetup(setup) 的
				// SDKConfig.ProxyURL 构造，代理取账号代理、不回退全局，且不污染全局 cfg。
				effective := h.configForOAuthSetup(setup)
				if effective == nil {
					t.Fatalf("%s effective config missing", tc.provider)
				}
				if effective.SDKConfig.ProxyURL != oauthAccountProxy {
					t.Fatalf("%s effective proxy = %q, want account proxy %q (must not fall back to global)", tc.provider, effective.SDKConfig.ProxyURL, oauthAccountProxy)
				}
				if h.cfg.SDKConfig.ProxyURL != oauthGlobalProxy {
					t.Fatalf("%s: global core proxy was mutated to %q; account proxy leaked into global cfg", tc.provider, h.cfg.SDKConfig.ProxyURL)
				}
			default:
				t.Fatalf("unknown exchangeVia %q", tc.exchangeVia)
			}

			// 落库侧：proxy 必须写进 record.ProxyURL 和 metadata。
			h.applyOAuthAccountSetupToRecord(tc.record, setup)
			if tc.record.ProxyURL != oauthAccountProxy {
				t.Fatalf("%s record.ProxyURL = %q, want %q", tc.provider, tc.record.ProxyURL, oauthAccountProxy)
			}
			if got, _ := tc.record.Metadata["proxy_url"].(string); got != oauthAccountProxy {
				t.Fatalf("%s record.Metadata[proxy_url] = %q, want %q", tc.provider, got, oauthAccountProxy)
			}
		})
	}
}

// TestOAuthSetupWiringIsNoopWithoutProxy 证明「不传 proxy」时接线是无害的：
// configForOAuthSetup 原样返回全局 cfg、prepareOAuthSetupRuntimeAuth 返回 nil
// （newClaudeOAuthAuth 因此退回 claude.NewClaudeAuth(h.cfg)），既有行为不变。
func TestOAuthSetupWiringIsNoopWithoutProxy(t *testing.T) {
	h := newProxyWiringTestHandler(t)

	if got := h.configForOAuthSetup(nil); got != h.cfg {
		t.Fatalf("configForOAuthSetup(nil) should return the global cfg unchanged, got %p want %p", got, h.cfg)
	}
	for _, provider := range []string{"claude", "codex", "antigravity", "xai", "kimi"} {
		if got := h.prepareOAuthSetupRuntimeAuth(provider, nil); got != nil {
			t.Fatalf("%s: prepareOAuthSetupRuntimeAuth with nil setup should be nil, got %#v", provider, got)
		}
	}
}
