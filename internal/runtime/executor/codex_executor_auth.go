package executor

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func resolveCodexAuthDiskPath(cfg *config.Config, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	var candidates []string
	fileBound := false
	if len(auth.Attributes) > 0 {
		if p := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); p != "" {
			candidates = append(candidates, p)
			fileBound = true
		}
		if s := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeSource]); s != "" && !strings.Contains(s, ":") {
			candidates = append(candidates, s)
		}
	}
	for _, name := range []string{strings.TrimSpace(auth.FileName), strings.TrimSpace(auth.ID)} {
		if name == "" {
			continue
		}
		if filepath.IsAbs(name) {
			candidates = append(candidates, name)
		} else if cfg != nil && strings.TrimSpace(cfg.AuthDir) != "" {
			candidates = append(candidates, filepath.Join(strings.TrimSpace(cfg.AuthDir), name))
		}
	}
	for i, candidate := range candidates {
		boundPath := fileBound && i == 0
		info, errStat := os.Stat(candidate)
		if errStat != nil {
			if os.IsNotExist(errStat) {
				if boundPath {
					return "", cliproxyauth.ErrSelectedRefreshUnavailable
				}
				continue
			}
			return "", fmt.Errorf("codex executor: stat credential file %s: %w", candidate, errStat)
		}
		if boundPath && !info.Mode().IsRegular() {
			return "", cliproxyauth.ErrSelectedRefreshUnavailable
		}
		if !info.IsDir() {
			canonical, errCanonical := filepath.EvalSymlinks(candidate)
			if errCanonical != nil {
				return "", fmt.Errorf("codex executor: resolve credential path: %w", errCanonical)
			}
			return filepath.Abs(canonical)
		}
	}
	return "", nil
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if e == nil {
		return nil
	}
	return resolveCodexKeyConfig(e.cfg, auth)
}

func resolveCodexKeyConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || cfg == nil {
		return nil
	}
	key := strings.TrimSpace(auth.Attributes["api_key"])
	if key == "" {
		return nil
	}
	selected, err := ordinaryCodexOrigin(strings.TrimSpace(auth.Attributes["base_url"]))
	if err != nil {
		return nil
	}
	matches := func(entry *config.CodexKey) bool {
		if strings.TrimSpace(entry.APIKey) != key {
			return false
		}
		origin, err := ordinaryCodexOrigin(strings.TrimSpace(entry.BaseURL))
		return err == nil && selected.Scheme == origin.Scheme &&
			strings.EqualFold(selected.Hostname(), origin.Hostname()) &&
			ordinaryCodexOriginPort(selected) == ordinaryCodexOriginPort(origin) &&
			strings.TrimRight(selected.EscapedPath(), "/") == strings.TrimRight(origin.EscapedPath(), "/")
	}
	rawIndex := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeConfigIndex])
	if rawIndex != "" {
		if index, err := strconv.Atoi(rawIndex); err == nil && index >= 0 && index < len(cfg.CodexKey) && matches(&cfg.CodexKey[index]) {
			return &cfg.CodexKey[index]
		}
	}
	var match *config.CodexKey
	for index := range cfg.CodexKey {
		entry := &cfg.CodexKey[index]
		if matches(entry) {
			if match != nil {
				return nil
			}
			match = entry
		}
	}
	return match
}

func (e *CodexExecutor) resolveCodexModelIsCompat(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, baseModel string) bool {
	if modelInfo, ok := cliproxyauth.ResolvedModelInfo(req); ok && modelInfo != nil {
		return modelInfo.IsCompat
	}
	entry := e.resolveCodexConfig(auth)
	if entry != nil && len(entry.Models) > 0 {
		requested := strings.TrimSpace(req.Model)
		target := strings.TrimSpace(baseModel)
		for i := range entry.Models {
			name := strings.TrimSpace(entry.Models[i].Name)
			alias := strings.TrimSpace(entry.Models[i].Alias)
			if (target != "" && (strings.EqualFold(name, target) || strings.EqualFold(alias, target))) ||
				(requested != "" && (strings.EqualFold(name, requested) || strings.EqualFold(alias, requested))) {
				return entry.Models[i].IsCompat
			}
		}
		return false
	}
	if options, configured := cliproxyauth.ResolvedHomeModelOptions(req); configured {
		return options.IsCompat
	}
	return false
}
