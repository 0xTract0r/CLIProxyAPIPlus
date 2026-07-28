package auth

import (
	"path/filepath"
	"strings"
)

// ResolveFarmPinAuthID resolves a farm account pin value to a unique auth ID.
//
// The pin value carried by a gated farm request (see the X-Farm-Account-Pin
// handler wiring) may be either the exact auth ID or a human friendly account
// name (auth label, OAuth email, email local-part, or the auth file base name).
// The exact auth ID is always the most robust value; name matching is a
// convenience for operators/orchestrators that only track account names.
//
// It returns (id, true) only when the value maps to exactly one auth. A value
// that matches zero auths, or is ambiguous across multiple auths, yields
// ("", false) so the caller never silently guesses which account to serve.
//
// Resolution is intentionally read-only and has no effect on selection by
// itself: it merely translates a name into the ID consumed by the existing
// pinned_auth_id fail-closed selection primitive.
func (m *Manager) ResolveFarmPinAuthID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if m == nil || value == "" {
		return "", false
	}
	// Fast path: an exact auth ID is unambiguous by construction.
	if _, ok := m.GetByID(value); ok {
		return value, true
	}
	matched := ""
	count := 0
	for _, auth := range m.List() {
		if farmPinAuthMatchesName(auth, value) {
			if auth.ID != matched {
				count++
			}
			matched = auth.ID
		}
	}
	if count == 1 {
		return matched, true
	}
	return "", false
}

// farmPinAuthMatchesName reports whether the given auth is identified by the
// supplied farm pin name. Name comparisons are case-insensitive; the exact ID
// comparison stays case-sensitive to match the selector's ID equality.
func farmPinAuthMatchesName(auth *Auth, value string) bool {
	if auth == nil {
		return false
	}
	if auth.ID == value {
		return true
	}
	if auth.Label != "" && strings.EqualFold(auth.Label, value) {
		return true
	}
	if email := farmPinAuthEmail(auth); email != "" {
		if strings.EqualFold(email, value) {
			return true
		}
		if at := strings.IndexByte(email, '@'); at > 0 && strings.EqualFold(email[:at], value) {
			return true
		}
	}
	if base := farmPinAuthFileBaseName(auth); base != "" && strings.EqualFold(base, value) {
		return true
	}
	return false
}

// farmPinAuthEmail returns the OAuth email recorded for the auth, if any.
func farmPinAuthEmail(auth *Auth) string {
	if auth == nil || len(auth.Metadata) == 0 {
		return ""
	}
	if v, ok := auth.Metadata["email"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// farmPinAuthFileBaseName returns the auth file base name without its extension
// (e.g. "/data/auths/daylenaldmin193.json" -> "daylenaldmin193"). Farm account
// names frequently mirror the backing auth file name.
func farmPinAuthFileBaseName(auth *Auth) string {
	if auth == nil {
		return ""
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		return ""
	}
	base := filepath.Base(name)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return strings.TrimSpace(base)
}
