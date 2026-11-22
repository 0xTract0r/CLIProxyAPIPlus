package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MoeForge/moego/json"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

// Manager aggregates authenticators and coordinates persistence via a token store.
type Manager struct {
	authenticators map[string]Authenticator
	store          coreauth.Store
}

// NewManager constructs a manager with the provided token store and authenticators.
// If store is nil, the caller must set it later using SetStore.
func NewManager(store coreauth.Store, authenticators ...Authenticator) *Manager {
	mgr := &Manager{
		authenticators: make(map[string]Authenticator),
		store:          store,
	}
	for i := range authenticators {
		mgr.Register(authenticators[i])
	}
	return mgr
}

// Register adds or replaces an authenticator keyed by its provider identifier.
func (m *Manager) Register(a Authenticator) {
	if a == nil {
		return
	}
	if m.authenticators == nil {
		m.authenticators = make(map[string]Authenticator)
	}
	m.authenticators[a.Provider()] = a
}

// SetStore updates the token store used for persistence.
func (m *Manager) SetStore(store coreauth.Store) {
	m.store = store
}

// Login executes the provider login flow and persists the resulting auth record.
func (m *Manager) Login(ctx context.Context, provider string, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, string, error) {
	auth, ok := m.authenticators[provider]
	if !ok {
		return nil, "", fmt.Errorf("cliproxy auth: authenticator %s not registered", provider)
	}

	loginProxyURL := ""
	if opts != nil {
		loginProxyURL = strings.TrimSpace(opts.ProxyURL)
	}
	if loginProxyURL != "" {
		if !proxyutil.ValidRequestProxy(loginProxyURL) {
			return nil, "", fmt.Errorf("cliproxy auth: invalid login proxy URL %q", proxyutil.Redact(loginProxyURL))
		}
		cfg = configWithLoginProxy(cfg, loginProxyURL)
	}

	record, err := auth.Login(ctx, cfg, opts)
	if err != nil {
		return nil, "", err
	}
	if record == nil {
		return nil, "", fmt.Errorf("cliproxy auth: authenticator %s returned nil record", provider)
	}

	store := m.storeForConfig(cfg)
	if store == nil {
		return record, "", nil
	}

	xaiSave, err := PrepareXAILoginForConfig(ctx, store, record, cfg)
	if err != nil {
		return record, "", err
	}
	if xaiSave != nil {
		coreauth.MergeExistingAuthMetadata(record, xaiSave.PriorMetadata)
	} else if cfg != nil {
		if strings.TrimSpace(cfg.AuthDir) != "" {
			targetFile := record.FileName
			if targetFile == "" {
				targetFile = record.ID
			}
			if targetFile != "" {
				fullPath := filepath.Join(cfg.AuthDir, targetFile)
				if raw, errRead := os.ReadFile(fullPath); errRead == nil && len(raw) > 0 {
					var existingMap map[string]any
					if errUnmarshal := json.Unmarshal(raw, &existingMap); errUnmarshal == nil && len(existingMap) > 0 {
						coreauth.MergeExistingAuthMetadata(record, existingMap)
					}
				}
			}
		}
	}
	legacyClaudeCredential, errLegacy := claudeauth.FindMatchingLegacyCredential(ctx, store, record)
	if errLegacy != nil {
		return record, "", errLegacy
	}
	if legacyClaudeCredential != nil {
		coreauth.MergeExistingAuthMetadata(record, legacyClaudeCredential.Metadata)
	}
	applyLoginProxyToRecord(record, loginProxyURL)

	savedPath, err := xaiSave.Save(ctx, store, record)
	if err != nil {
		return record, "", err
	}
	if legacyClaudeCredential != nil {
		if strings.TrimSpace(savedPath) == "" {
			return record, "", fmt.Errorf("cliproxy auth: canonical Claude credential was not persisted; legacy credential retained")
		}
		legacyID := strings.TrimSpace(legacyClaudeCredential.ID)
		if legacyID == "" {
			legacyID = strings.TrimSpace(legacyClaudeCredential.FileName)
		}
		if errDelete := store.Delete(ctx, legacyID); errDelete != nil {
			return record, savedPath, fmt.Errorf("cliproxy auth: canonical Claude credential saved but legacy credential cleanup failed: %w", errDelete)
		}
	}
	return record, savedPath, xaiSave.Finish(ctx, savedPath)
}

func configWithLoginProxy(cfg *config.Config, proxyURL string) *config.Config {
	if cfg == nil {
		return &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxyURL}}
	}
	cloned := cfg.CloneForRuntime()
	if cloned == nil {
		copied := *cfg
		cloned = &copied
	}
	cloned.ProxyURL = proxyURL
	return cloned
}

func applyLoginProxyToRecord(record *coreauth.Auth, proxyURL string) {
	if record == nil || proxyURL == "" {
		return
	}
	record.ProxyURL = proxyURL
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	record.Metadata["proxy_url"] = proxyURL
	if setter, ok := record.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		setter.SetMetadata(record.Metadata)
	}
}

func metadataStringValue(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

// adoptLegacyXAIFileName points a subject-only xAI record at the existing
// email-named credential so the save refreshes that file in place. The token
// storage email is backfilled because SaveTokenToFile merges the record
// metadata over the serialized storage and would otherwise drop the email.
// Path attributes pointing at the superseded subject-named file are cleared
// so the store resolves the adopted name.
func (m *Manager) SaveAuth(record *coreauth.Auth, cfg *config.Config) (string, error) {
	store := m.storeForConfig(cfg)
	if store == nil {
		return "", fmt.Errorf("no store configured")
	}
	return store.Save(context.Background(), record)
}

func (m *Manager) storeForConfig(cfg *config.Config) coreauth.Store {
	store := m.store
	if cfg != nil {
		if files, ok := store.(*FileTokenStore); ok {
			return files.WithBaseDir(cfg.AuthDir)
		}
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
	}
	return store
}
