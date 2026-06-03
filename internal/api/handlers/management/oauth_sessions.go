package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	oauthCallbackWaitTimeout    = 15 * time.Minute
	oauthSessionTTL             = oauthCallbackWaitTimeout + 5*time.Minute
	maxOAuthStateLength         = 128
	oauthSessionStatusComplete  = "ok"
	oauthSessionStatusCancelled = "cancelled"
)

var (
	errInvalidOAuthState      = errors.New("invalid oauth state")
	errUnsupportedOAuthFlow   = errors.New("unsupported oauth provider")
	errOAuthSessionNotPending = errors.New("oauth session is not pending")
)

type oauthSession struct {
	Provider    string
	Status      string
	SavedPath   string
	AuthName    string
	Note        string
	ProxyURL    string
	CompletedAt time.Time
	CreatedAt   time.Time
	ExpiresAt   time.Time
	cancel      context.CancelFunc
}

type oauthSessionStore struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]oauthSession
}

func newOAuthSessionStore(ttl time.Duration) *oauthSessionStore {
	if ttl <= 0 {
		ttl = oauthSessionTTL
	}
	return &oauthSessionStore{
		ttl:      ttl,
		sessions: make(map[string]oauthSession),
	}
}

func (s *oauthSessionStore) purgeExpiredLocked(now time.Time) {
	for state, session := range s.sessions {
		if !session.ExpiresAt.IsZero() && now.After(session.ExpiresAt) {
			delete(s.sessions, state)
		}
	}
}

func (s *oauthSessionStore) Register(state, provider string) {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if state == "" || provider == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	s.sessions[state] = oauthSession{
		Provider:  provider,
		Status:    "",
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
}

func (s *oauthSessionStore) SetError(state, message string) {
	state = strings.TrimSpace(state)
	message = strings.TrimSpace(message)
	if state == "" {
		return
	}
	if message == "" {
		message = "Authentication failed"
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return
	}
	if isOAuthSessionCancelledStatus(session.Status) || isOAuthSessionCompleteStatus(session.Status) {
		return
	}
	session.Status = message
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
}

func (s *oauthSessionStore) Complete(state string) {
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	delete(s.sessions, state)
}

func (s *oauthSessionStore) CompleteWithResult(state string, result oauthSessionResult) {
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return
	}
	session.Status = oauthSessionStatusComplete
	session.SavedPath = strings.TrimSpace(result.SavedPath)
	session.AuthName = strings.TrimSpace(result.AuthName)
	session.Note = strings.TrimSpace(result.Note)
	session.ProxyURL = strings.TrimSpace(result.ProxyURL)
	if provider := strings.ToLower(strings.TrimSpace(result.Provider)); provider != "" {
		session.Provider = provider
	}
	session.CompletedAt = now
	session.ExpiresAt = now.Add(s.ttl)
	session.cancel = nil
	s.sessions[state] = session
}

func (s *oauthSessionStore) Cancel(state string) bool {
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	now := time.Now()
	var cancel context.CancelFunc

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		s.mu.Unlock()
		return false
	}
	cancel = session.cancel
	session.cancel = nil
	session.Status = oauthSessionStatusCancelled
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

func (s *oauthSessionStore) CompleteProvider(provider string) int {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return 0
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	removed := 0
	for state, session := range s.sessions {
		if strings.EqualFold(session.Provider, provider) {
			if isOAuthSessionCancelledStatus(session.Status) || isOAuthSessionCompleteStatus(session.Status) {
				continue
			}
			delete(s.sessions, state)
			removed++
		}
	}
	return removed
}

func (s *oauthSessionStore) SetCancel(state string, cancel context.CancelFunc) bool {
	state = strings.TrimSpace(state)
	if state == "" || cancel == nil {
		return false
	}
	now := time.Now()
	cancelImmediately := false

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || isOAuthSessionCancelledStatus(session.Status) {
		cancelImmediately = true
	} else {
		session.cancel = cancel
		s.sessions[state] = session
	}
	s.mu.Unlock()

	if cancelImmediately {
		cancel()
		return false
	}
	return true
}

func (s *oauthSessionStore) Get(state string) (oauthSession, bool) {
	state = strings.TrimSpace(state)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	return session, ok
}

func (s *oauthSessionStore) IsPending(state, provider string) bool {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return false
	}
	if session.Status != "" {
		if !strings.EqualFold(session.Provider, "kiro") {
			return false
		}
		if !strings.HasPrefix(session.Status, "device_code|") && !strings.HasPrefix(session.Status, "auth_url|") {
			return false
		}
	}
	if provider == "" {
		return true
	}
	return strings.EqualFold(session.Provider, provider)
}

var oauthSessions = newOAuthSessionStore(oauthSessionTTL)

func RegisterOAuthSession(state, provider string) { oauthSessions.Register(state, provider) }

func SetOAuthSessionError(state, message string) { oauthSessions.SetError(state, message) }

func CompleteOAuthSession(state string) { oauthSessions.Complete(state) }

func CompleteOAuthSessionWithResult(state string, result oauthSessionResult) {
	oauthSessions.CompleteWithResult(state, result)
}

func CancelOAuthSessionState(state string) bool { return oauthSessions.Cancel(state) }

func CompleteOAuthSessionsByProvider(provider string) int {
	return oauthSessions.CompleteProvider(provider)
}

func SetOAuthSessionCancel(state string, cancel context.CancelFunc) bool {
	return oauthSessions.SetCancel(state, cancel)
}

func GetOAuthSession(state string) (provider string, status string, ok bool) {
	session, ok := oauthSessions.Get(state)
	if !ok {
		return "", "", false
	}
	return session.Provider, session.Status, true
}

func IsOAuthSessionPending(state, provider string) bool {
	return oauthSessions.IsPending(state, provider)
}

func isOAuthSessionCancelledStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), oauthSessionStatusCancelled)
}

func isOAuthSessionCompleteStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), oauthSessionStatusComplete)
}

func oauthSessionErrorWithCause(message string, cause error) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Authentication failed"
	}
	if cause == nil {
		return message
	}
	detail := strings.TrimSpace(cause.Error())
	if detail == "" {
		return message
	}
	return message + ": " + detail
}

func ValidateOAuthState(state string) error {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", errInvalidOAuthState)
	}
	if len(trimmed) > maxOAuthStateLength {
		return fmt.Errorf("%w: too long", errInvalidOAuthState)
	}
	if strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\") {
		return fmt.Errorf("%w: contains path separator", errInvalidOAuthState)
	}
	if strings.Contains(trimmed, "..") {
		return fmt.Errorf("%w: contains '..'", errInvalidOAuthState)
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("%w: invalid character", errInvalidOAuthState)
		}
	}
	return nil
}

func NormalizeOAuthProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "anthropic", nil
	case "codex", "openai":
		return "codex", nil
	case "gitlab":
		return "gitlab", nil
	case "gemini", "google":
		return "gemini", nil
	case "antigravity", "anti-gravity":
		return "antigravity", nil
	case "kiro":
		return "kiro", nil
	case "github":
		return "github", nil
	case "xai", "x-ai", "x.ai", "grok":
		return "xai", nil
	default:
		return "", errUnsupportedOAuthFlow
	}
}

type oauthCallbackFilePayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

func WriteOAuthCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	if strings.TrimSpace(authDir) == "" {
		return "", fmt.Errorf("auth dir is empty")
	}
	canonicalProvider, err := NormalizeOAuthProvider(provider)
	if err != nil {
		return "", err
	}
	if err := ValidateOAuthState(state); err != nil {
		return "", err
	}

	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", canonicalProvider, state)
	filePath := filepath.Join(authDir, fileName)
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", fmt.Errorf("create oauth callback dir: %w", err)
	}
	payload := oauthCallbackFilePayload{
		Code:  strings.TrimSpace(code),
		State: strings.TrimSpace(state),
		Error: strings.TrimSpace(errorMessage),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal oauth callback payload: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return "", fmt.Errorf("write oauth callback file: %w", err)
	}
	return filePath, nil
}

func WriteOAuthCallbackFileForPendingSession(authDir, provider, state, code, errorMessage string) (string, error) {
	canonicalProvider, err := NormalizeOAuthProvider(provider)
	if err != nil {
		return "", err
	}
	if !IsOAuthSessionPending(state, canonicalProvider) {
		return "", errOAuthSessionNotPending
	}
	return WriteOAuthCallbackFile(authDir, canonicalProvider, state, code, errorMessage)
}
