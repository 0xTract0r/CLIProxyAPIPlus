package auth

import (
	"net/http"
	"strings"
	"time"
)

// Fork-only anti-burn safety net: after a rolling window of terminal
// authentication failures with zero intervening successes, a credential is
// automatically quarantined so the scheduler stops hammering a revoked/invalid
// token (which can trip provider reuse detection and accelerate account
// suspension). A single real success always lifts the quarantine, so a
// legitimate revoke->reauth cycle recovers immediately. Ported into the
// upstream v7.2.101 split-file structure from the fork conductor monolith; the
// call sites are woven into conductor_cooldown.go MarkResult (evaluate) and
// conductor_lifecycle.go Update (stale-writeback guard).
const (
	// authAutoQuarantineWindow is the rolling window used to detect a terminal
	// auth/permission failure streak with zero successes in between (see
	// evaluateAutoQuarantineLocked). It sits in the middle of the reviewed
	// 30-60 minute range: long enough to tolerate a couple of low-frequency
	// probe cycles (e.g. a telemetry-farm keepalive firing every ~30-90min)
	// without over-reacting to a single flaky 401, short enough that a truly
	// revoked-token account is quarantined well before it accumulates a large
	// amount of wasted 30-minute cooldown/retry cycles.
	authAutoQuarantineWindow = 45 * time.Minute
	// authAutoQuarantineFailureThreshold is the minimum number of terminal
	// auth failures (with zero successes in between) inside
	// authAutoQuarantineWindow before the credential is automatically
	// quarantined.
	authAutoQuarantineFailureThreshold = 2
	// quarantineReasonTerminalAuthFailure is the sanitized classification code
	// persisted as Auth.QuarantineReason. It never echoes the raw upstream
	// error body (which is not guaranteed to be free of sensitive content),
	// mirroring the sanitization already used for terminal refresh failures.
	quarantineReasonTerminalAuthFailure = "terminal_auth_failure"

	// The following three keys mirror the AutoQuarantined/QuarantineReason/
	// QuarantinedAt struct fields (see Auth in types.go, whose json tags use
	// the identical names) into the persisted auth.Metadata map. This is the
	// only representation that actually survives a process restart: auth
	// JSON files and Postgres auth_store rows both only ever serialize
	// auth.Metadata (see sdk/auth/filestore.go Save / internal/store/
	// postgresstore.go Save), never the top-level Auth struct itself. Without
	// this mirror, markAutoQuarantine's struct-field write is purely
	// in-memory and a terminal quarantine silently evaporates on the next
	// restart (readAuthFiles / postgresstore.List would see a plain,
	// unquarantined record) -- the exact gap this const block,
	// setAutoQuarantineMetadata and clearAutoQuarantineMetadata close. See
	// readAuthFiles (sdk/auth/filestore.go) and
	// applyQuarantineStateFromMetadata (internal/store/postgresstore.go) for
	// the corresponding restore-on-load side.
	metadataKeyAutoQuarantined  = "auto_quarantined"
	metadataKeyQuarantineReason = "quarantine_reason"
	metadataKeyQuarantinedAt    = "quarantined_at"
)

// setAutoQuarantineMetadata writes the persisted terminal-auth quarantine
// lock into auth.Metadata (creating the map if necessary) so it survives a
// process restart, mirroring markAutoQuarantine's struct-field write. Callers
// must hold m.mu (or otherwise own exclusive access to auth), same as every
// other mutator of auth.Metadata in this package.
func setAutoQuarantineMetadata(auth *Auth, reason string, at time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataKeyAutoQuarantined] = true
	auth.Metadata[metadataKeyQuarantineReason] = reason
	auth.Metadata[metadataKeyQuarantinedAt] = at.UTC().Format(time.RFC3339)
}

// clearAutoQuarantineMetadata removes the persisted quarantine lock keys from
// auth.Metadata, mirroring clearAutoQuarantine's struct-field reset. It is a
// no-op when auth.Metadata is nil or the keys are already absent, matching
// clearAutoQuarantine's own idempotent behavior.
func clearAutoQuarantineMetadata(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, metadataKeyAutoQuarantined)
	delete(auth.Metadata, metadataKeyQuarantineReason)
	delete(auth.Metadata, metadataKeyQuarantinedAt)
}

// isLongContextExtraUsageRequiredMessage reports whether an error message is the
// Claude "extra usage is required for long context requests" signal.
func isLongContextExtraUsageRequiredMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "extra usage is required for long context requests")
}

// isLongContextExtraUsageRequiredResultError reports whether a failure result is
// the Claude long-context extra-usage 429. It is treated as a per-request,
// non-terminal signal (the credential itself is healthy), so it must never be
// counted toward the auto-quarantine streak.
func isLongContextExtraUsageRequiredResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusTooManyRequests {
		return false
	}
	return isLongContextExtraUsageRequiredMessage(err.Message)
}

// isTerminalAuthQuarantineResultError reports whether a failure result
// represents a terminal authentication/permission failure (e.g. a revoked
// OAuth token or invalid credentials returning HTTP 401 authentication_error)
// as opposed to a transient error that can recover on its own: rate limiting
// (429), overload/gateway errors (408/5xx), quota exhaustion, a model-support
// gap, or the other already-specialized failure classes handled earlier in
// MarkResult. Only terminal auth failures count toward the automatic
// quarantine rolling window (see evaluateAutoQuarantineLocked); everything
// else must keep following the existing per-status-code cooldown/retry path
// unchanged.
//
// This intentionally classifies by HTTP status (401) rather than by matching
// substrings like "revoked" in the raw error body: the existing MarkResult
// switch already isolates 401 into its own "unauthorized" cooldown case,
// distinct from 402/403 (payment_required), 404, 429 (quota), and
// 408/500/502/503/504 (transient upstream). Reusing that same boundary keeps
// this classifier robust across providers whose exact wording for a revoked
// credential varies, instead of depending on a fragile message-content match.
func isTerminalAuthQuarantineResultError(err *Error) bool {
	if err == nil {
		return false
	}
	// These failure classes already have their own dedicated recovery paths
	// and must never be double-counted as a permission revocation, even
	// though some of them can (rarely) surface alongside a 401-shaped error.
	if isCloudflareChallengeResultError(err) ||
		isModelSupportResultError(err) ||
		isRequestScopedNotFoundResultError(err) ||
		isLongContextExtraUsageRequiredResultError(err) {
		return false
	}
	return statusCodeFromResult(err) == http.StatusUnauthorized
}

// evaluateAutoQuarantineLocked maintains the rolling terminal-auth-failure
// streak for auth and flips Auth.AutoQuarantined once the streak reaches
// authAutoQuarantineFailureThreshold within authAutoQuarantineWindow with
// zero intervening successes. It must be called once per MarkResult
// invocation, after any other status/state mutation for this result, so it
// is always the final word on AutoQuarantined/Status for this call and can
// never be silently clobbered by the generic "auth.Status = StatusError"
// writes that the existing per-status-code branches perform for every kind
// of failure (429/5xx included). Callers must hold m.mu.
//
// A real success (success=true) — including a per-model success that merely
// tripped a quota limit — always resets the streak and lifts any existing
// quarantine: it proves the credential itself is valid, which is the exact
// "reauth 后一次真实成功请求即可解除隔离" recovery signal this feature must
// preserve (an account can legitimately cycle through revoke->reauth several
// times and must never be permanently blacklisted by this heuristic alone).
func (m *Manager) evaluateAutoQuarantineLocked(auth *Auth, success bool, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	if success {
		auth.terminalAuthFailureStreak = 0
		auth.terminalAuthFailureStreakStartAt = time.Time{}
		if auth.AutoQuarantined {
			clearAutoQuarantine(auth, now)
		}
		return
	}
	if !isTerminalAuthQuarantineResultError(resultErr) {
		// Transient/other failures (429, 5xx, timeouts, quota, model support,
		// ...) neither advance nor reset the terminal-auth streak: they are
		// not a success, so an in-progress streak must survive them, but they
		// also are not themselves evidence of a revoked credential.
		return
	}
	if auth.terminalAuthFailureStreak <= 0 || now.Sub(auth.terminalAuthFailureStreakStartAt) > authAutoQuarantineWindow {
		auth.terminalAuthFailureStreak = 1
		auth.terminalAuthFailureStreakStartAt = now
		return
	}
	auth.terminalAuthFailureStreak++
	if auth.terminalAuthFailureStreak >= authAutoQuarantineFailureThreshold && !auth.AutoQuarantined {
		markAutoQuarantine(auth, now)
	}
}

// markAutoQuarantine sets the persisted AutoQuarantined lock. Callers must
// hold m.mu (or otherwise own exclusive access to auth).
func markAutoQuarantine(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.AutoQuarantined = true
	auth.QuarantineReason = quarantineReasonTerminalAuthFailure
	auth.QuarantinedAt = now
	auth.Status = StatusQuarantined
	auth.StatusMessage = "auto_quarantined: repeated authentication failures, credential needs re-authentication"
	auth.Unavailable = true
	// The credential is skipped entirely by isAuthBlockedForModel while
	// quarantined (like StatusDisabled), so a NextRetryAfter-driven cooldown
	// retry would never fire anyway; clearing it just keeps the persisted
	// state from implying a retry is still scheduled.
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
	// See preserveQuarantineFieldsOnStaleWriteback: stamp the quarantine
	// freshness clock so a stale write-back can be detected and rejected.
	auth.quarantineStateAt = now
	// Persist the lock into Metadata so it survives a restart (see
	// setAutoQuarantineMetadata doc comment for why this is required).
	setAutoQuarantineMetadata(auth, quarantineReasonTerminalAuthFailure, now)
}

// clearAutoQuarantine releases the AutoQuarantined lock and resets the streak
// bookkeeping. Callers must hold m.mu (or otherwise own exclusive access to
// auth), except when invoked via the exported Auth.ClearAutoQuarantine
// wrapper used by callers outside this package that already own the record
// exclusively (e.g. a freshly built re-auth record not yet shared with the
// manager).
func clearAutoQuarantine(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	wasQuarantined := auth.AutoQuarantined
	auth.AutoQuarantined = false
	auth.QuarantineReason = ""
	auth.QuarantinedAt = time.Time{}
	auth.terminalAuthFailureStreak = 0
	auth.terminalAuthFailureStreakStartAt = time.Time{}
	// See preserveQuarantineFieldsOnStaleWriteback: stamp the quarantine
	// freshness clock unconditionally, even when this call is idempotent
	// (already unquarantined) -- callers like saveTokenRecord call this on
	// every reauth regardless of prior state, and the clear is still the
	// caller's authoritative, current intent for this field.
	auth.quarantineStateAt = now
	// Mirror the release into Metadata too (see clearAutoQuarantineMetadata):
	// unconditional and idempotent for the same reason as the struct-field
	// reset above, so a restart never resurrects a lock that was already
	// lifted (a completed reauth, or an operator re-enable).
	clearAutoQuarantineMetadata(auth)
	if auth.Status == StatusQuarantined {
		auth.Status = StatusActive
		auth.StatusMessage = ""
		auth.Unavailable = false
	}
	if wasQuarantined {
		auth.UpdatedAt = now
	}
}

// preserveQuarantineFieldsOnStaleWriteback guards the automatic terminal-auth
// quarantine lock (AutoQuarantined / QuarantineReason / QuarantinedAt, plus
// the in-memory streak bookkeeping) against being silently rolled back by a
// stale in-flight clone, the same way preserveNewerTokenOwnedFields guards
// token material. It cannot use a plain "existing is quarantined => force
// copy" rule like Manager.Update already applies to Success/Failed/
// recentRequests, because the two sanctioned recovery paths -- a completed
// reauth (saveTokenRecord -> Auth.ClearAutoQuarantine) and an explicit
// operator re-enable (PatchAuthFileStatus / PatchAuthFileAccountSettings ->
// Auth.ClearAutoQuarantine) -- legitimately need to CLEAR the lock, and their
// cleared end state (AutoQuarantined=false, QuarantineReason="",
// QuarantinedAt=zero) is byte-for-byte identical to a stale clone that was
// taken before the quarantine was ever set on the live entry.
//
// The two cases are told apart by Auth.quarantineStateAt, an in-memory-only
// freshness clock that markAutoQuarantine/clearAutoQuarantine stamp to the
// real wall-clock time on every call (mark or clear) and that Auth.Clone
// preserves unchanged (like LastRefreshedAt for tokens) -- so it correctly
// survives an internal re-clone within the same request (e.g.
// syncAuthManagedHeaderState building a fresh Auth to attach new metadata to)
// without losing the caller's already-current quarantine intent, while still
// detecting a clone that was taken strictly before the live entry's most
// recent mark/clear. A same-or-newer incoming quarantineStateAt is trusted
// as-is (whatever it says: quarantined or explicitly cleared); a strictly
// older one means the incoming record predates the live entry's last
// quarantine decision and must not be allowed to roll it back.
//
// Returns whether the incoming record's quarantine fields were overwritten.
// Callers must hold m.mu (called only from Manager.Update).
func preserveQuarantineFieldsOnStaleWriteback(incoming, existing *Auth) bool {
	if incoming == nil || existing == nil {
		return false
	}
	if !existing.quarantineStateAt.After(incoming.quarantineStateAt) {
		return false
	}
	changed := incoming.AutoQuarantined != existing.AutoQuarantined ||
		incoming.QuarantineReason != existing.QuarantineReason ||
		!incoming.QuarantinedAt.Equal(existing.QuarantinedAt)
	if !changed {
		return false
	}
	incoming.AutoQuarantined = existing.AutoQuarantined
	incoming.QuarantineReason = existing.QuarantineReason
	incoming.QuarantinedAt = existing.QuarantinedAt
	incoming.terminalAuthFailureStreak = existing.terminalAuthFailureStreak
	incoming.terminalAuthFailureStreakStartAt = existing.terminalAuthFailureStreakStartAt
	incoming.quarantineStateAt = existing.quarantineStateAt
	if existing.AutoQuarantined {
		// Keep the restored record internally consistent with the exact
		// "quarantined" view markAutoQuarantine produces, instead of only
		// restoring the AutoQuarantined bool while leaving
		// Status/Unavailable/NextRetryAfter at whatever the stale clone
		// happened to carry. Without this, a stale write-back could still
		// produce the same class of self-contradictory persisted state
		// (AutoQuarantined=true but Status/Unavailable disagreeing) that the
		// refreshAuthStatus success-path fix addresses for the other
		// direction (see management.refreshAuthStatus).
		incoming.Status = existing.Status
		incoming.StatusMessage = existing.StatusMessage
		incoming.Unavailable = existing.Unavailable
		incoming.NextRetryAfter = existing.NextRetryAfter
		// Manager.Update persists `incoming` right after this guard runs (see
		// conductor_lifecycle.go), and only incoming.Metadata is ever
		// serialized to disk/Postgres -- restoring the struct fields above
		// without also restoring the mirrored Metadata keys would let a
		// stale write-back's unaware Metadata (missing the lock) reach disk
		// even though the in-memory struct fields now correctly say
		// quarantined, silently losing the lock again on the very next
		// restart.
		setAutoQuarantineMetadata(incoming, existing.QuarantineReason, existing.QuarantinedAt)
	} else {
		// existing was explicitly cleared (a completed reauth or operator
		// re-enable): mirror that into incoming.Metadata too, so a stale
		// write-back landing right after a legitimate clear does not
		// resurrect stale quarantine Metadata keys on the next persist/List.
		clearAutoQuarantineMetadata(incoming)
	}
	return true
}
