package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// IncrementCyberPolicyCount records a provider refusal for the operator.
// Updates use the same per-credential mutation lock as administrative writes.
func (m *Manager) IncrementCyberPolicyCount(ctx context.Context, authID string) (int, time.Time, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0, time.Time{}, nil
	}
	release, err := m.lockAuthMutationContext(ctx, authID)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer release()
	now := time.Now().UTC()
	m.mu.Lock()
	existing, ok := m.auths[authID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return 0, time.Time{}, nil
	}
	// Perform the read-modify-write under the lock: mutate the in-memory entry
	// directly so concurrent hits cannot lose increments.
	existing.CyberPolicyFlagCount++
	existing.LastCyberPolicyAt = now
	newCount := existing.CyberPolicyFlagCount
	snapshot := existing.Clone()
	m.mu.Unlock()
	if snapshot == nil {
		return newCount, now, nil
	}
	// Persist outside the lock: persist no longer acquires m.mu, but keep it out
	// of the critical section for safety. Surface the error so callers can decide
	// whether to suppress the webhook dispatch.
	if err := m.persist(ctx, snapshot); err != nil {
		return newCount, now, err
	}
	return newCount, now, nil
}

const (
	codexCreditsHasCreditsHeader = "X-Codex-Credits-Has-Credits"
	codexCreditsBalanceHeader    = "X-Codex-Credits-Balance"
	codexCreditsUnlimitedHeader  = "X-Codex-Credits-Unlimited"
)

func codexCreditsUsable(headers http.Header) bool {
	if len(headers) == 0 {
		return false
	}
	if !codexHeaderTrue(headers.Get(codexCreditsHasCreditsHeader)) {
		return false
	}
	if codexHeaderTrue(headers.Get(codexCreditsUnlimitedHeader)) {
		return true
	}
	raw := strings.TrimSpace(headers.Get(codexCreditsBalanceHeader))
	if raw == "" {
		return false
	}
	balance, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(balance) || math.IsInf(balance, 0) {
		return false
	}
	return balance > 0
}

// CodexCreditsUsable decodes the reported credit balance for SDK consumers.
func CodexCreditsUsable(headers http.Header) bool {
	return codexCreditsUsable(headers)
}

func codexHeaderTrue(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "true")
}
