package auth

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// SetRetryConfig updates additional retry rounds, the per-round credential cap, and cooldown wait.
//
// fork(harden-account-scheduling-limiter ERR-2): maxRetryCredentials default wiring.
// yaml cannot distinguish an omitted max-retry-credentials from an explicit 0, so both
// land here as 0 and now default to internalconfig.DefaultMaxRetryCredentials (2) --
// bounding the cross-credential failover blast radius of one failed request instead of
// the legacy "walk every available credential" behavior. A negative value (e.g. -1) is
// the explicit escape hatch for that legacy unbounded behavior: it is stored as 0, which
// conductor_execution.go's `maxRetryCredentials > 0` cap check already treats as
// unbounded. A positive value is used as-is. Negative values must reach this call
// un-clamped -- see the matching fork notes in internal/config/parse.go and
// internal/config/config_load.go.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	switch {
	case maxRetryCredentials == 0:
		maxRetryCredentials = internalconfig.DefaultMaxRetryCredentials
	case maxRetryCredentials < 0:
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	var toReschedule []string
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	for id, auth := range m.auths {
		if auth != nil && strings.EqualFold(executorKeyFromAuth(auth), provider) {
			toReschedule = append(toReschedule, id)
		}
	}
	m.mu.Unlock()

	for _, id := range toReschedule {
		m.queueRefreshReschedule(id)
	}

	if replaced == nil || replaced == executor {
		return
	}
	if sharer, ok := replaced.(ExecutionSessionSharer); ok && sharer != nil && sharer.SharesExecutionSessionsWith(executor) {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.register(ctx, auth, false)
}

// RegisterReauthenticated opens a new registration after a completed login was
// persisted. Stable account identity and usage history survive this credential
// replacement; callers must independently validate that recovery is authorized.
func (m *Manager) RegisterReauthenticated(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.register(ctx, auth, true)
}

func (m *Manager) register(ctx context.Context, auth *Auth, reauthenticated bool) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	// Fork: hydrate runtime-only fields (per-account proxy_url, managed/custom
	// headers) from metadata before the scheduler and executors observe the auth.
	hydrateRuntimeFields(auth)
	NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("register auth: %w", errWeight)
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, auth.ID)
	if errMutation != nil {
		return nil, errMutation
	}
	return m.registerLocked(ctx, auth, reauthenticated, releaseMutation)
}

// registerLocked publishes while the caller owns the reload/credential gate.
// The release function is idempotent and is run before reentrant hooks.
func (m *Manager) registerLocked(ctx context.Context, auth *Auth, reauthenticated bool, releaseMutation func()) (*Auth, error) {
	defer releaseMutation()
	now := time.Now()
	if auth.Generation == 0 {
		auth.Generation = 1
	}
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = now
	}
	auth.UpdatedAt = now
	cooldownStateChanged := normalizeModelStates(auth)
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	auth.EnsureIndex()
	m.mu.Lock()
	if reauthenticated {
		if existing := m.auths[auth.ID]; existing != nil && !ReauthenticationIdentityMatches(existing, auth) {
			m.mu.Unlock()
			return nil, fmt.Errorf("reauthenticate auth %s: account identity changed", auth.ID)
		}
	}
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if existing, exists := m.auths[auth.ID]; exists && existing != nil && existing.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = existing.RegistrationEpoch
	}
	if auth.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = auth.RegistrationEpoch
	}
	m.authEpochs[auth.ID]++
	auth.RegistrationEpoch = m.authEpochs[auth.ID]
	if existing, exists := m.auths[auth.ID]; exists && existing != nil {
		if reauthenticated {
			auth.CreatedAt = existing.CreatedAt
			auth.Index, auth.indexAssigned = existing.Index, existing.indexAssigned
			auth.Success, auth.Failed = existing.Success, existing.Failed
			auth.recentRequests = existing.recentRequests
		}
		auth.CredentialVersion = max(auth.CredentialVersion, existing.CredentialVersion) + 1
	} else if auth.CredentialVersion == 0 {
		auth.CredentialVersion = 1
	}
	auth.Generation = 1
	// Serialize this credential, but release the manager lock during store I/O.
	// Persist failures stay non-fatal, but must not be silent: a restart would
	// lose the credential that only exists in memory.
	if errPersist := m.persistLocked(ctx, auth); errPersist != nil {
		log.WithFields(log.Fields{"auth_id": auth.ID, "credential": auth.ID, "provider": auth.Provider}).Warnf("failed to persist registered auth %s (%s): %v", auth.Provider, auth.ID, errPersist)
	}
	// Persistence is serialized by the credential gate. Unrelated credentials
	// must not wait on pacing while a store is blocked. Acquire pacing only for
	// publication and policy reconciliation, always before the manager mutex.
	m.mu.Unlock()
	m.pacingMu.Lock()
	pacingLocked := true
	defer func() {
		if pacingLocked {
			m.pacingMu.Unlock()
		}
	}()
	m.mu.Lock()
	if m.authEpochs[auth.ID] != auth.RegistrationEpoch {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth %s changed registration during persistence", auth.ID)
	}
	auth.ClearFileSnapshot()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.notifyAuthChangeLocked(auth.ID)
	// Snapshot before unlocking: MarkResult mutates the published auth in place.
	var schedulerSnapshot *Auth
	if m.scheduler != nil {
		schedulerSnapshot = authClone.Clone()
	}
	m.mu.Unlock()
	m.refreshWarmupPacingLocked()
	m.pacingMu.Unlock()
	pacingLocked = false
	releaseMutation()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	m.structuralEpoch.Add(1)
	m.queueRefreshReschedule(auth.ID)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
	return auth.Clone(), nil
}

// CommitReauthenticated validates and persists a complete replacement under the
// same credential gate as ordinary edits and refresh commits. prepare must
// independently authorize the replacement from the submitted login material;
// callbacks may read this manager but must not mutate it or run lifecycle hooks.
// A persistence failure never advances the registration or publishes recovery.
func (m *Manager) CommitReauthenticated(ctx context.Context, id string, expectedEpoch uint64, prepare func(*Auth) (*Auth, error), persist func(context.Context, *Auth) error) (*Auth, error) {
	if m == nil || strings.TrimSpace(id) == "" || prepare == nil || persist == nil {
		return nil, fmt.Errorf("reauthenticate: incomplete transaction")
	}
	releaseMutation, err := m.lockAuthMutationContext(ctx, id)
	if err != nil {
		return nil, err
	}
	defer releaseMutation()
	m.mu.RLock()
	var current *Auth
	if stored := m.auths[id]; stored != nil {
		current = stored.Clone()
	}
	m.mu.RUnlock()
	if current == nil || (expectedEpoch != 0 && current.RegistrationEpoch != expectedEpoch) {
		return nil, fmt.Errorf("reauthenticate auth %s: missing or stale registration", id)
	}
	next, err := prepare(current.Clone())
	if err != nil {
		return nil, err
	}
	if next == nil || !ReauthenticationIdentityMatches(current, next) {
		return nil, fmt.Errorf("reauthenticate auth %s: account identity changed", id)
	}
	hydrateRuntimeFields(next)
	NormalizeCredentialMetadata(next.Metadata)
	if err := ValidateAuthWeight(next); err != nil {
		return nil, err
	}
	next.CreatedAt = current.CreatedAt
	next.Index, next.indexAssigned = current.Index, current.indexAssigned
	next.RegistrationEpoch, next.CredentialVersion = current.RegistrationEpoch, current.CredentialVersion
	releaseSave, err := m.beginForegroundStoreSave(ctx)
	if err != nil {
		return nil, err
	}
	err = persist(ctx, next)
	releaseSave()
	if err != nil {
		return nil, err
	}
	// Persistence has committed. Reuse normal publication without saving twice
	// or reacquiring the credential gate; ordinary hooks run after its release.
	return m.registerLocked(WithSkipPersist(ctx), next, true, releaseMutation)
}

// CommitNonRecoveryUpdate strictly persists an ordinary same-account edit without
// opening a new registration or releasing terminal/operator/quota protections.
// prepare receives the latest snapshot under the credential gate and must validate
// all submitted credential identity claims before merging them. persist must make
// an atomic save; neither callback may mutate this manager or invoke its hooks.
// A nonzero expected credential version fences stale partial credential edits.
func (m *Manager) CommitNonRecoveryUpdate(ctx context.Context, id string, expectedEpoch, expectedCredentialVersion uint64, prepare func(*Auth) (*Auth, error), persist func(context.Context, *Auth) error) (*Auth, error) {
	if m == nil || strings.TrimSpace(id) == "" || prepare == nil || persist == nil {
		return nil, fmt.Errorf("non-recovery update: incomplete transaction")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	releaseMutation, err := m.lockAuthMutationContext(ctx, id)
	if err != nil {
		return nil, err
	}
	defer releaseMutation()
	m.mu.RLock()
	var current *Auth
	if stored := m.auths[id]; stored != nil {
		current = cloneNonRecoveryAuth(stored)
	}
	m.mu.RUnlock()
	if current == nil || (expectedEpoch != 0 && current.RegistrationEpoch != expectedEpoch) || (expectedCredentialVersion != 0 && current.CredentialVersion != expectedCredentialVersion) {
		return nil, fmt.Errorf("non-recovery update auth %s: missing or stale credential", id)
	}
	prepared, err := prepare(cloneNonRecoveryAuth(current))
	if err != nil {
		return nil, err
	}
	if prepared == nil || !ReauthenticationIdentityMatches(current, prepared) {
		return nil, fmt.Errorf("non-recovery update auth %s: account identity changed", id)
	}
	// This content merge starts from current, retaining runtime status, quota,
	// model errors, counters, rejected tokens, registration and stable identity.
	next := MergePreparedAuth(current, current, prepared)
	NormalizeCredentialMetadata(next.Metadata)
	preserveNonRecoveryMetadata(next, current)
	if err := ValidateAuthWeight(next); err != nil {
		return nil, err
	}
	next.CredentialVersion = current.CredentialVersion
	if CredentialsChanged(current, next) {
		next.CredentialVersion++
	}
	next.Generation = current.Generation + 1
	next.UpdatedAt = time.Now()
	releaseSave, err := m.beginForegroundStoreSave(ctx)
	if err != nil {
		return nil, err
	}
	saved := cloneNonRecoveryAuth(next)
	err = persist(ctx, saved)
	releaseSave()
	if err != nil {
		return nil, err
	}
	// Store callbacks may backfill content, but cannot own manager identity,
	// revisions, history, terminal status or quota. Isolate their delta exactly
	// as persistLocked does, then retain only the ordinary content projection.
	storeResult := cloneNonRecoveryAuth(next)
	mergeAuthSaveDelta(storeResult, next, saved, true)
	// Verify known claims after storage backfill, before publication.
	if !nonRecoveryStoredIdentityMatches(next, storeResult) {
		return nil, fmt.Errorf("non-recovery update auth %s: store changed account identity", id)
	}
	next = MergePreparedAuth(next, next, storeResult)
	preserveNonRecoveryMetadata(next, current)
	next.CredentialVersion = current.CredentialVersion
	if CredentialsChanged(current, next) {
		next.CredentialVersion++
	}
	// Store I/O holds only the per-account gate. Runtime observations that use
	// m.mu directly remain live and their intervening delta wins publication.
	m.pacingMu.Lock()
	m.mu.Lock()
	latest := m.auths[id]
	if latest == nil || latest.RegistrationEpoch != current.RegistrationEpoch || latest.CredentialVersion != current.CredentialVersion {
		m.mu.Unlock()
		m.pacingMu.Unlock()
		return nil, fmt.Errorf("non-recovery update auth %s: credential changed during persistence", id)
	}
	mergeAuthSaveDelta(next, current, latest, false)
	if latest.Generation >= next.Generation {
		next.Generation = latest.Generation + 1
	}
	next.ClearFileSnapshot()
	m.auths[id] = cloneNonRecoveryAuth(next)
	m.notifyAuthChangeLocked(id)
	committed := cloneNonRecoveryAuth(next)
	m.mu.Unlock()
	m.refreshWarmupPacingLocked()
	m.pacingMu.Unlock()
	releaseMutation()
	m.finishAuthUpdate(ctx, cloneNonRecoveryAuth(committed), false)
	return committed, nil
}

// cloneNonRecoveryAuth isolates mutable terminal errors at transaction boundaries.
// Opaque Storage/Runtime retain their existing ownership contract: callbacks must
// not mutate manager-owned objects in place.
func cloneNonRecoveryAuth(a *Auth) *Auth {
	out := a.Clone()
	if out != nil && a.LastError != nil {
		lastError := *a.LastError
		out.LastError = &lastError
	}
	return out
}

// Missing claims may be filled by storage; known claims cannot be replaced or
// removed. This rejects publication, not external side effects of a bad Store.
func nonRecoveryStoredIdentityMatches(before, after *Auth) bool {
	for _, key := range []string{"account_id", "chatgpt_account_id", "email"} {
		known, _ := before.Metadata[key].(string)
		known = strings.TrimSpace(known)
		if known == "" {
			continue
		}
		value, ok := after.Metadata[key].(string)
		value = strings.TrimSpace(value)
		if !ok || (key == "email" && !strings.EqualFold(known, value)) || (key != "email" && known != value) {
			return false
		}
	}
	return true
}

// ReauthenticationIdentityMatches rejects a reused filename carrying a different
// provider/account before it can inherit account history or operator metadata.
func ReauthenticationIdentityMatches(existing, incoming *Auth) bool {
	if existing == nil || incoming == nil || existing.ID != incoming.ID || !strings.EqualFold(strings.TrimSpace(existing.Provider), strings.TrimSpace(incoming.Provider)) {
		return false
	}
	matchedAccount := false
	for _, key := range []string{"account_id", "chatgpt_account_id"} {
		before, _ := existing.Metadata[key].(string)
		after, _ := incoming.Metadata[key].(string)
		before, after = strings.TrimSpace(before), strings.TrimSpace(after)
		if before != "" && after != "" {
			if before != after {
				return false
			}
			matchedAccount = true
		}
	}
	if matchedAccount {
		return true
	}
	before, _ := existing.Metadata["email"].(string)
	after, _ := incoming.Metadata["email"].(string)
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)
	return before == "" || after == "" || strings.EqualFold(before, after)
}

type updateAuthMode int

const (
	updateModeReplace updateAuthMode = iota
	updateModeRefresh
	updateModePrepare
)

// UpdatePreparedAuth atomically merges request preparation results into the latest runtime auth
// under the manager lock, preserving concurrent modifications without modifying refresh lifecycle fields.
func (m *Manager) UpdatePreparedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	return m.updateInternal(ctx, base, updated, updateModePrepare)
}

// UpdateRefreshedAuth atomically merges refresh results into the latest runtime auth
// under the manager lock, preserving concurrent modifications (proxy_url, notes, weights, etc.).
func (m *Manager) UpdateRefreshedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	return m.updateInternal(ctx, base, updated, updateModeRefresh)
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	return m.updateInternal(ctx, nil, auth, updateModeReplace)
}

// Modify loads the latest auth for id while holding the per-auth mutation gate,
// applies edit to a clone of that auth, and commits the result through the
// standard persistence and publication path. It returns (nil, false, nil) if
// the auth is not present when the gate is acquired.
func (m *Manager) Modify(ctx context.Context, id string, expectedEpoch uint64, edit func(*Auth) error) (*Auth, bool, error) {
	id = strings.TrimSpace(id)
	if m == nil || id == "" || edit == nil {
		return nil, false, nil
	}
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, id)
	if errMutation != nil {
		return nil, false, errMutation
	}
	defer releaseMutation()

	m.mu.RLock()
	existing, ok := m.auths[id]
	var current *Auth
	var currentEpoch uint64
	if ok && existing != nil {
		current = existing.Clone()
		currentEpoch = existing.RegistrationEpoch
		if m.authEpochs != nil && m.authEpochs[id] > currentEpoch {
			currentEpoch = m.authEpochs[id]
		}
	}
	m.mu.RUnlock()
	if current == nil {
		return nil, false, nil
	}
	if expectedEpoch != 0 && expectedEpoch < currentEpoch {
		return nil, false, fmt.Errorf("update auth %s: stale registration epoch %d < %d", id, expectedEpoch, currentEpoch)
	}
	if errEdit := edit(current); errEdit != nil {
		return nil, false, errEdit
	}
	if expectedEpoch != 0 {
		current.RegistrationEpoch = expectedEpoch
	}
	committedAuth, didCommit, persistCooldown, errUpdate := m.updateInternalLocked(ctx, nil, current, updateModeReplace)
	releaseMutation()
	if errUpdate == nil && didCommit && committedAuth != nil {
		m.finishAuthUpdate(ctx, committedAuth, persistCooldown)
	}
	return committedAuth, errUpdate == nil && didCommit && committedAuth != nil, errUpdate
}

func (m *Manager) updateInternal(ctx context.Context, base, auth *Auth, mode updateAuthMode) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	releaseMutation, errMutation := m.lockAuthMutationContext(ctx, auth.ID)
	if errMutation != nil {
		return nil, errMutation
	}
	committedAuth, didCommit, persistCooldown, err := m.updateInternalLocked(ctx, base, auth, mode)
	releaseMutation()
	if err == nil && didCommit && committedAuth != nil {
		m.finishAuthUpdate(ctx, committedAuth, persistCooldown)
	}
	return committedAuth, err
}

// finishAuthUpdate runs post-commit side effects after the per-auth mutation
// gate has been released, preserving the original update ordering in which
// hooks and scheduler updates could re-enter manager operations.
func (m *Manager) finishAuthUpdate(ctx context.Context, committed *Auth, persistCooldown bool) {
	if committed == nil {
		return
	}
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(committed.Clone())
	}
	m.structuralEpoch.Add(1)
	m.queueRefreshReschedule(committed.ID)
	m.hook.OnAuthUpdated(ctx, committed.Clone())
	if persistCooldown {
		m.persistCooldownStates(context.Background())
	}
}

// updateInternalLocked assumes the caller already holds the per-auth
// mutation lock. It only commits manager state; callers run
// finishAuthUpdate after releasing the gate. persistCooldownStates is also
// deferred by callers because it may re-enter manager operations.
// updateInternalLocked commits an update while the caller holds the gate.
// The second return value reports whether this call actually committed new
// state (versus returning the current snapshot for a discarded stale
// update); only real commits may run post-commit side effects.
func (m *Manager) updateInternalLocked(ctx context.Context, base, auth *Auth, mode updateAuthMode) (committedAuth *Auth, committed bool, persistCooldown bool, err error) {
	if auth == nil || auth.ID == "" {
		return nil, false, false, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, false, false, fmt.Errorf("update auth: %w", errWeight)
	}
	isFileSnapshot := mode == updateModeReplace && auth.HasFileSnapshot()
	if isFileSnapshot {
		m.mu.RLock()
		existing, ok := m.auths[auth.ID]
		m.mu.RUnlock()
		if !ok || existing == nil {
			return nil, false, false, nil
		}
		// Validate file snapshot freshness under the per-auth mutation gate
		// without holding m.mu across filesystem I/O.
		if !auth.FileSnapshotMatchesDisk() {
			m.mu.RLock()
			current := m.auths[auth.ID].Clone()
			m.mu.RUnlock()
			return current, false, false, nil
		}
	}
	hydrateRuntimeFields(auth)
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return nil, false, false, nil
	}
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if existing.RegistrationEpoch > m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = existing.RegistrationEpoch
	}
	if (mode == updateModeRefresh || mode == updateModePrepare) && base != nil && existing.RegistrationEpoch != base.RegistrationEpoch {
		m.mu.Unlock()
		return nil, false, false, fmt.Errorf("update auth %s: stale registration epoch %d != %d", auth.ID, base.RegistrationEpoch, existing.RegistrationEpoch)
	}
	// Do not let an in-flight refresh overwrite credentials committed after its snapshot.
	if mode == updateModeRefresh && base != nil && (existing.CredentialVersion != base.CredentialVersion || CredentialsChanged(base, existing)) {
		if auth.CodexRefreshPatch == nil || !auth.CodexRefreshPatch.CanApplyTo(existing.Metadata) {
			current := existing.Clone()
			m.mu.Unlock()
			// Discarded stale refresh: committed=false so callers do not
			// run finishAuthUpdate for a state this call did not change.
			return current, false, false, nil
		}
	}
	if mode == updateModeRefresh {
		merged := MergeRefreshedAuth(base, existing, auth)
		if merged != nil {
			auth = merged
			NormalizeCredentialMetadata(auth.Metadata)
		}
	} else if mode == updateModePrepare {
		merged := MergePreparedAuth(base, existing, auth)
		if merged != nil {
			auth = merged
			NormalizeCredentialMetadata(auth.Metadata)
		}
	}
	if auth.RegistrationEpoch != 0 && auth.RegistrationEpoch < m.authEpochs[auth.ID] {
		m.mu.Unlock()
		return nil, false, false, fmt.Errorf("update auth %s: stale registration epoch %d < %d", auth.ID, auth.RegistrationEpoch, m.authEpochs[auth.ID])
	}
	if auth.RegistrationEpoch >= m.authEpochs[auth.ID] {
		m.authEpochs[auth.ID] = auth.RegistrationEpoch
	} else if auth.RegistrationEpoch == 0 {
		auth.RegistrationEpoch = m.authEpochs[auth.ID]
	}
	if !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	if isFileSnapshot && auth.CreatedAt.IsZero() {
		auth.CreatedAt = existing.CreatedAt
	}
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = existing.recentRequests
	if auth.Generation <= existing.Generation {
		auth.Generation = existing.Generation + 1
	} else {
		auth.Generation++
	}
	existingVersion := existing.CredentialVersion
	if existingVersion == 0 {
		existingVersion = 1
	}
	credChanged := CredentialsChanged(existing, auth)
	if credChanged {
		auth.CredentialVersion = max(auth.CredentialVersion, existingVersion) + 1
	} else {
		auth.CredentialVersion = existingVersion
	}
	// Guard against a stale write-back silently rolling back the automatic
	// terminal-auth quarantine lock: a caller that cloned this auth before a
	// concurrent MarkResult quarantined it would otherwise wholesale-overwrite
	// the live entry with its unaware zero-value quarantine fields.
	if preserveQuarantineFieldsOnStaleWriteback(auth, existing) {
		logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warn("auth update carried stale auto-quarantine fields; preserved live quarantine state")
	}
	cooldownStateChanged := false
	if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
		if isFileSnapshot {
			auth.ModelStates = existing.ModelStates
			auth.LastRefreshedAt = existing.LastRefreshedAt
			auth.NextRefreshAfter = existing.NextRefreshAfter
			auth.RefreshFailures = existing.RefreshFailures
		} else if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
			auth.ModelStates = existing.ModelStates
		}
		if isFileSnapshot {
			if (auth.Status == "" || auth.Status == StatusActive) && existing.Status != "" {
				auth.Status = existing.Status
			}
			if auth.StatusMessage == "" {
				auth.StatusMessage = existing.StatusMessage
			}
			auth.Unavailable = existing.Unavailable
			auth.NextRetryAfter = existing.NextRetryAfter
			if auth.LastError == nil && existing.LastError != nil {
				auth.LastError = cloneError(existing.LastError)
			}
			observation := mergeQuotaObservation(auth.Quota, existing.Quota)
			auth.Quota = existing.Quota.Clone()
			auth.Quota.ObservedAt = observation.ObservedAt
			auth.Quota.Signals = observation.Signals
			auth.cooldownRecordedSeq = existing.cooldownRecordedSeq
		}
		if credChanged || mode == updateModeRefresh {
			applyPreservedRejectedTokens(existing, auth, mode == updateModeRefresh)
			if !auth.AutoQuarantined && (auth.RejectedAccessToken == "" || authAccessToken(auth) != auth.RejectedAccessToken) {
				if hasUnauthorizedAuthFailure(existing) || (auth.LastError != nil && isUnauthorizedError(auth.LastError)) {
					auth.Unavailable = false
					auth.NextRetryAfter = time.Time{}
					auth.cooldownRecordedSeq = 0
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
					cooldownStateChanged = true
				}
				resumed := clearUnauthorizedModelStates(auth, time.Now())
				if len(resumed) > 0 {
					cooldownStateChanged = true
				}
			}
		} else {
			applyPreservedRejectedTokens(existing, auth, false)
		}
		// A refresh that confirms quota restoration (issue #6690) releases the
		// credential-wide cooldown early; otherwise the active credential
		// quota below is preserved exactly as before.
		devinQuotaRestored := mode == updateModeRefresh && applyDevinQuotaRestoration(auth, existing, time.Now())
		if devinQuotaRestored {
			cooldownStateChanged = true
		} else if existing.Quota.Exceeded && existing.Quota.Reason == "credential_quota" && existing.Quota.NextRecoverAt.After(time.Now()) {
			auth.Unavailable = existing.Unavailable
			auth.NextRetryAfter = existing.NextRetryAfter
			// Preserve cooldown fields; keep the newest observation snapshot.
			observation := mergeQuotaObservation(auth.Quota, existing.Quota)
			auth.Quota = existing.Quota
			auth.Quota.ObservedAt = observation.ObservedAt
			auth.Quota.Signals = observation.Signals
			auth.cooldownRecordedSeq = existing.cooldownRecordedSeq
			if auth.Status == StatusActive {
				auth.Status = existing.Status
			}
		}
	}
	// Guard against a stale write-back rolling token state backwards: if a
	// refresh advanced the stored tokens after the caller cloned this auth,
	// keep the newer token-owned fields while still applying the caller's
	// non-token metadata changes.
	if preserveNewerTokenOwnedFields(auth, existing) {
		logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warn("auth update carried stale token-owned fields; preserved newer stored token state")
	}
	now := time.Now()
	auth.UpdatedAt = now
	cooldownStateChanged = normalizeModelStates(auth) || cooldownStateChanged
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	if existing.Metadata != nil {
		if persistedHints, hasHints := existing.Metadata["antigravity_capability_hints"]; hasHints {
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["antigravity_capability_hints"] = persistedHints
		}
	}
	hydrateRuntimeFields(auth)
	auth.EnsureIndex()
	// Save before publication, including the transactional Meta mint path.
	// Runtime-only changes made during I/O are merged below, never overwritten.
	existingBeforeSave := existing.Clone()
	refreshPatchBeforeSave := auth.CodexRefreshPatch
	persistMetaMint := (mode == updateModePrepare || mode == updateModeRefresh) && strings.EqualFold(strings.TrimSpace(auth.Provider), "meta")
	if errPersist := m.persistLocked(ctx, auth); errPersist != nil {
		// A minted Meta key must reach the store before requests can use it.
		if persistMetaMint {
			m.mu.Unlock()
			return nil, false, false, fmt.Errorf("persist meta auth: %w", errPersist)
		}
		// Ordinary persistence failures remain non-fatal, but never silent.
		log.WithFields(log.Fields{"auth_id": auth.ID, "credential": auth.ID, "provider": auth.Provider}).Warnf("failed to persist updated auth %s (%s): %v", auth.Provider, auth.ID, errPersist)
	}
	// The mutation gate keeps this credential stable across store I/O; pace
	// only the final publication so a slow Save cannot block unrelated auths.
	m.mu.Unlock()
	m.pacingMu.Lock()
	defer m.pacingMu.Unlock()
	m.mu.Lock()
	if m.authEpochs[auth.ID] != auth.RegistrationEpoch {
		m.mu.Unlock()
		return nil, false, false, fmt.Errorf("auth %s changed registration during persistence", auth.ID)
	}
	mergeAuthSaveDelta(auth, existingBeforeSave, existing, false)
	if (auth.RejectedAccessToken == "" && existing.RejectedAccessToken != "") || (auth.RejectedSIWCAccessToken == "" && existing.RejectedSIWCAccessToken != "") {
		probe := auth.Clone()
		probe.CodexRefreshPatch = refreshPatchBeforeSave
		applyPreservedRejectedTokens(existing, probe, mode == updateModeRefresh)
		auth.RejectedAccessToken = probe.RejectedAccessToken
		auth.RejectedSIWCAccessToken = probe.RejectedSIWCAccessToken
	}
	if existing.Generation >= auth.Generation {
		auth.Generation = existing.Generation + 1
	}
	auth.ClearFileSnapshot()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.notifyAuthChangeLocked(auth.ID)
	// Snapshot before unlocking: MarkResult mutates the published auth in place.
	var schedulerSnapshot *Auth
	if m.scheduler != nil {
		schedulerSnapshot = authClone.Clone()
	}
	m.mu.Unlock()
	m.refreshWarmupPacingLocked()
	_ = schedulerSnapshot
	return auth.Clone(), true, cooldownStateChanged, nil
}

// Remove deletes an auth from runtime state without persisting.
// Disk and token-store deletion must be handled by the caller.
func (m *Manager) Remove(ctx context.Context, id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	_ = ctx
	releaseMutation := m.lockAuthMutation(id)
	defer releaseMutation()

	m.pacingMu.Lock()
	m.mu.Lock()
	existing := m.auths[id]
	if existing == nil {
		m.mu.Unlock()
		m.pacingMu.Unlock()
		return
	}
	provider := strings.TrimSpace(existing.Provider)
	delete(m.auths, id)
	m.notifyAuthChangeLocked(id)
	if m.modelPoolOffsets != nil {
		delete(m.modelPoolOffsets, id)
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64)
	}
	if existing.RegistrationEpoch > m.authEpochs[id] {
		m.authEpochs[id] = existing.RegistrationEpoch
	}
	m.authEpochs[id]++
	tombstoneEpoch := m.authEpochs[id]
	m.mu.Unlock()
	m.pacingMu.Unlock()
	releaseMutation()

	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.RecordRemovalTombstone(id, tombstoneEpoch)
	}
	m.structuralEpoch.Add(1)
	m.queueRefreshUnschedule(id)
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if authCloser, okAuthCloser := exec.(AuthExecutionSessionCloser); okAuthCloser && authCloser != nil {
				authCloser.CloseAuthExecutionSessions(id)
			} else if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStates(context.Background())
}

func (m *Manager) invalidateSessionAffinity(authID string) {
	if m == nil || authID == "" {
		return
	}
	sel := m.Selector()
	if invalidator, ok := sel.(interface{ InvalidateAuth(string) }); ok && invalidator != nil {
		invalidator.InvalidateAuth(authID)
	}
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	// Exclude complete Save/publication transactions, including new registrations
	// not yet present in auths. Never wait for this barrier while holding m.mu.
	lockCtx := ctx
	if lockCtx == nil {
		lockCtx = context.Background()
	}
	if errAcquire := m.authLoadGate.Acquire(lockCtx, math.MaxInt64); errAcquire != nil {
		return errAcquire
	}
	defer m.authLoadGate.Release(math.MaxInt64)
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	items, err := store.List(ctx)
	if err != nil {
		return err
	}
	m.pacingMu.Lock()
	m.mu.Lock()
	previousAuths := m.auths
	m.auths = make(map[string]*Auth, len(items))
	if m.authEpochs == nil {
		m.authEpochs = make(map[string]uint64, len(items))
	}
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		NormalizeCredentialMetadata(auth.Metadata)
		if errWeight := ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		auth.EnsureIndex()
		m.authEpochs[auth.ID] = max(m.authEpochs[auth.ID], auth.RegistrationEpoch) + 1
		auth.RegistrationEpoch = m.authEpochs[auth.ID]
		auth.Generation = 1
		if prev, exists := previousAuths[auth.ID]; exists && prev != nil {
			auth.CredentialVersion = max(auth.CredentialVersion, prev.CredentialVersion)
			if CredentialsChanged(prev, auth) {
				auth.CredentialVersion++
			}
		}
		if auth.CredentialVersion == 0 {
			auth.CredentialVersion = 1
		}
		m.auths[auth.ID] = auth.Clone()
	}

	type removalTombstone struct {
		id    string
		epoch uint64
	}
	var removedTombstones []removalTombstone
	for prevID := range previousAuths {
		if _, exists := m.auths[prevID]; !exists {
			m.authEpochs[prevID]++
			removedTombstones = append(removedTombstones, removalTombstone{
				id:    prevID,
				epoch: m.authEpochs[prevID],
			})
		}
	}

	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	for id := range m.authChangeWatchers {
		m.notifyAuthChangeLocked(id)
	}
	m.mu.Unlock()
	m.refreshWarmupPacingLocked()
	m.pacingMu.Unlock()

	if m.scheduler != nil {
		for _, rt := range removedTombstones {
			m.scheduler.RecordRemovalTombstone(rt.id, rt.epoch)
		}
	}
	m.structuralEpoch.Add(1)
	m.syncScheduler()
	return nil
}

type authPersistLock struct {
	mu             sync.Mutex
	lastEpoch      uint64
	lastGeneration uint64
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil || auth == nil {
		return nil
	}
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return fmt.Errorf("persist auth: %w", errWeight)
	}
	if IsConfigAPIKeyAuth(auth) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	if IsPluginVirtualAuth(auth) {
		return nil
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}

	lockVal, _ := m.persistLocks.LoadOrStore(auth.ID, &authPersistLock{})
	pLock, _ := lockVal.(*authPersistLock)
	if pLock != nil {
		pLock.mu.Lock()
		defer pLock.mu.Unlock()
		if auth.RegistrationEpoch < pLock.lastEpoch || (auth.RegistrationEpoch == pLock.lastEpoch && auth.Generation < pLock.lastGeneration) {
			return nil
		}
		pLock.lastEpoch = auth.RegistrationEpoch
		pLock.lastGeneration = auth.Generation
		if shouldSkipPersist(ctx) {
			return nil
		}
		releaseFg, errWait := m.beginForegroundStoreSave(ctx)
		if errWait != nil {
			return errWait
		}
		defer releaseFg()
		_, err := store.Save(ctx, auth)
		return err
	}

	if shouldSkipPersist(ctx) {
		return nil
	}
	releaseFg, errWait := m.beginForegroundStoreSave(ctx)
	if errWait != nil {
		return errWait
	}
	defer releaseFg()
	_, err := store.Save(ctx, auth)
	return err
}
