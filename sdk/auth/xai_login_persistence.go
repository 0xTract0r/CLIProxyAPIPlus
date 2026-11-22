package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// XAILoginPersistence carries previously verified metadata and deferred local cleanup.
type XAILoginPersistence struct {
	PriorMetadata map[string]any
	destination   string
	expected      *xaiFileSnapshot
	cleanup       func(context.Context, string) error
}

type xaiLoginPlanKey struct{}

// ErrXAILoginFileChanged requests a retry after an ordinary concurrent change.
var ErrXAILoginFileChanged = errors.New("local xAI credential changed during login; retry")

// Save persists the verified plan through the existing store contract.
func (p *XAILoginPersistence) Save(ctx context.Context, store cliproxyauth.Store, record *cliproxyauth.Auth) (string, error) {
	ctx = cliproxyauth.WithAuthCreationIntent(ctx)
	if p != nil && p.destination != "" {
		ctx = context.WithValue(ctx, xaiLoginPlanKey{}, p)
	}
	return store.Save(ctx, record)
}

func sameXAIFileSnapshot(a, b *xaiFileSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return os.SameFile(a.info, b.info) && bytes.Equal(a.raw, b.raw)
}

func (p *XAILoginPersistence) verifyLocked(path string, current *xaiFileSnapshot) error {
	if p.destination != path || !sameXAIFileSnapshot(p.expected, current) {
		return ErrXAILoginFileChanged
	}
	return nil
}

// Finish removes a verified predecessor only after the destination save succeeded.
func (p *XAILoginPersistence) Finish(ctx context.Context, savedPath string) error {
	if p == nil || p.cleanup == nil {
		return nil
	}
	return p.cleanup(ctx, savedPath)
}

type xaiFileSnapshot struct {
	metadata map[string]any
	info     os.FileInfo
	raw      []byte
}

func readXAIFile(path string) (*xaiFileSnapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("xAI credential is not a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) || info.Size() != current.Size() || !info.ModTime().Equal(current.ModTime()) {
		return nil, cliproxyauth.ErrFileRemovalBusy
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil || !strings.EqualFold(metadataStringValue(metadata, "type"), "xai") {
		return nil, fmt.Errorf("xAI credential file has unknown contents")
	}
	return &xaiFileSnapshot{metadata: metadata, info: current, raw: raw}, nil
}

func xaiLoginDestination(path string, incoming map[string]any) (*xaiFileSnapshot, error) {
	existing, err := readXAIFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect xAI credential destination: %w", err)
	}
	if !sameReferenceXAIIdentity(incoming, existing.metadata) {
		return nil, fmt.Errorf("xAI credential destination has a different or unverified identity")
	}
	return existing, nil
}

func readExternalXAIExistingMetadata(cfg *config.Config, record *cliproxyauth.Auth) map[string]any {
	if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
		return nil
	}
	name := strings.TrimSpace(record.FileName)
	if name == "" {
		name = strings.TrimSpace(record.ID)
	}
	if filepath.Base(name) != name || !strings.HasSuffix(strings.ToLower(name), ".json") {
		return nil
	}
	target := filepath.Join(cfg.AuthDir, name)
	if explicit := strings.TrimSpace(record.Attributes[cliproxyauth.AttributePath]); explicit != "" {
		configuredPath, errTarget := filepath.Abs(target)
		explicitPath, errExplicit := filepath.Abs(explicit)
		if errTarget != nil || errExplicit != nil || explicitPath != configuredPath {
			return nil
		}
	}
	existing, err := xaiLoginDestination(target, record.Metadata)
	if err != nil || existing == nil {
		return nil
	}
	metadata := make(map[string]any, 2)
	for _, key := range []string{"note", "disabled"} {
		if value, ok := existing.metadata[key]; ok {
			metadata[key] = value
		}
	}
	return metadata
}

// PrepareXAILogin prepares ordinary local xAI persistence. Unknown storage
// layouts cannot safely authorize deletion and are left to the store.
func PrepareXAILogin(ctx context.Context, store cliproxyauth.Store, record *cliproxyauth.Auth) (*XAILoginPersistence, error) {
	return PrepareXAILoginForConfig(ctx, store, record, nil)
}

// PrepareXAILoginForConfig also inherits operator metadata from the verified
// destination in the configured auth directory when using an external store.
func PrepareXAILoginForConfig(ctx context.Context, store cliproxyauth.Store, record *cliproxyauth.Auth, cfg *config.Config) (*XAILoginPersistence, error) {
	if record == nil || !strings.EqualFold(record.Provider, "xai") {
		return nil, nil
	}
	plan := &XAILoginPersistence{}
	local, ok := store.(*FileTokenStore)
	if !ok {
		// Arbitrary stores define their own save consistency and may allow a
		// write even when enumeration is unavailable. Do not migrate there.
		plan.PriorMetadata = readExternalXAIExistingMetadata(cfg, record)
		return plan, nil
	}
	path, err := local.resolveAuthPath(record)
	if err != nil {
		return nil, err
	}
	existing, err := xaiLoginDestination(path, record.Metadata)
	if err != nil {
		return nil, err
	}
	plan.destination, err = ordinaryAuthWritePath(path)
	if err != nil {
		return nil, err
	}
	plan.expected = existing
	if existing != nil {
		// When two files already coexist, neither is known to be obsolete.
		plan.PriorMetadata = existing.metadata
		return plan, nil
	}
	email, subject := metadataStringValue(record.Metadata, "email"), metadataStringValue(record.Metadata, "sub")
	if email == "" && subject == "" {
		return plan, nil
	}
	name := xai.CredentialFileName(email, subject)
	if record.Attributes[cliproxyauth.AttributePath] != "" || record.FileName != name || record.ID != name {
		return plan, nil
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return plan, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var prior *xaiFileSnapshot
	var priorName string
	for _, entry := range entries {
		candidate := entry.Name()
		if !strings.HasPrefix(candidate, "xai-") || !strings.HasSuffix(candidate, ".json") || strings.EqualFold(candidate, name) {
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		found, err := readXAIFile(filepath.Join(dir, candidate))
		if err != nil || !sameReferenceXAIIdentity(record.Metadata, found.metadata) ||
			!isReferenceXAIPredecessor(candidate, found.metadata) {
			continue
		}
		if prior != nil {
			// Multiple possible predecessors can carry different operator settings.
			return plan, nil
		}
		prior, priorName = found, candidate
	}
	if prior == nil {
		return plan, nil
	}

	plan.PriorMetadata = prior.metadata
	// A subject-only login must not move a richer email-named record back
	// to a poorer name. Reuse an already canonical richer destination.
	if priorEmail := metadataStringValue(prior.metadata, "email"); email == "" && priorEmail != "" &&
		priorName == xai.CredentialFileName(priorEmail, subject) {
		if disabled, _ := record.Metadata["disabled"].(bool); disabled {
			// Keep the official refusal to rename an explicitly disabled destination.
			return plan, nil
		}
		record.ID, record.FileName = priorName, priorName
		record.Metadata["email"] = priorEmail
		if storage, ok := record.Storage.(*xai.TokenStorage); ok && storage != nil {
			storage.Email = priorEmail
		}
		plan.destination, plan.expected = filepath.Join(dir, priorName), prior
		return plan, nil
	}
	dest, old := filepath.Join(dir, name), filepath.Join(dir, priorName)
	plan.cleanup = func(ctx context.Context, savedPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(savedPath) == "" {
			return fmt.Errorf("canonical xAI credential was not persisted; predecessor retained")
		}
		savedDir, err := filepath.EvalSymlinks(filepath.Dir(savedPath))
		if err != nil || filepath.Join(savedDir, filepath.Base(savedPath)) != dest {
			return fmt.Errorf("xAI credential saved to an unexpected path; predecessor retained")
		}
		first, second := dest, old
		if second < first {
			first, second = second, first
		}
		err = withReferenceXAILock(first, func() error {
			for _, path := range []string{first, second} {
				info, err := os.Lstat(path)
				if errors.Is(err, os.ErrNotExist) && path == old {
					return nil
				}
				if err != nil || !info.Mode().IsRegular() {
					return fmt.Errorf("xAI migration path changed; predecessor retained")
				}
			}
			return withReferenceXAILock(second, func() error {
				saved, err := readXAIFile(dest)
				if err != nil || !sameReferenceXAIIdentity(prior.metadata, saved.metadata) {
					return fmt.Errorf("xAI destination could not be verified; predecessor retained")
				}
				current, err := readXAIFile(old)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil || os.SameFile(saved.info, current.info) || !os.SameFile(prior.info, current.info) ||
					!bytes.Equal(prior.raw, current.raw) {
					return fmt.Errorf("xAI predecessor changed during save; predecessor retained")
				}
				// Make the destination directory entry durable before removing the only
				// recognized predecessor. If the filesystem cannot sync, keep both.
				directory, err := os.Open(dir)
				if err != nil {
					return err
				}
				defer directory.Close()
				if err := directory.Sync(); err != nil {
					return err
				}
				return os.Remove(old)
			})
		})
		if err != nil {
			return fmt.Errorf("canonical xAI credential saved but predecessor cleanup was not completed: %w", err)
		}
		return nil
	}
	return plan, nil
}

func withReferenceXAILock(path string, f func() error) error {
	unlock := lockOrdinaryAuthWrite(path)
	defer unlock()
	return f()
}

func sameReferenceXAIIdentity(incoming, stored map[string]any) bool {
	email, priorEmail := metadataStringValue(incoming, "email"), metadataStringValue(stored, "email")
	sub, priorSub := metadataStringValue(incoming, "sub"), metadataStringValue(stored, "sub")
	if email != "" && priorEmail != "" && !strings.EqualFold(email, priorEmail) {
		return false
	}
	if sub != "" || priorSub != "" {
		return sub != "" && sub == priorSub
	}
	return email != "" && priorEmail != "" && strings.EqualFold(email, priorEmail)
}

func isReferenceXAIPredecessor(name string, stored map[string]any) bool {
	email, sub := metadataStringValue(stored, "email"), metadataStringValue(stored, "sub")
	return (email != "" && (strings.EqualFold(name, referenceXAILegacyName(email)) ||
		strings.EqualFold(name, xai.CredentialFileName(email, "")))) ||
		(sub != "" && (strings.EqualFold(name, referenceXAILegacyName(sub)) ||
			strings.EqualFold(name, xai.CredentialFileName("", sub))))
}

func referenceXAILegacyName(value string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._-", r) {
			return r
		}
		return '-'
	}, strings.TrimSpace(value))
	if safe = strings.Trim(safe, "-"); safe != "" {
		return "xai-" + safe + ".json"
	}
	return ""
}
