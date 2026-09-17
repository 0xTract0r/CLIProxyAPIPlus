package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const warmupServingCapacity = 4096

// Expiry evidence is bounded and short-lived. It is not a usable binding and
// does not extend session TTL: it only suppresses the extra reserve lottery.
func (c *SessionCache) rememberServingExpiryLocked(key string, entry sessionEntry) {
	if entry.serving == nil {
		return
	}
	now := time.Now()
	if c.servingExpired == nil {
		c.servingExpired = make(map[string]time.Time)
	}
	for id, until := range c.servingExpired {
		if !now.Before(until) {
			delete(c.servingExpired, id)
		}
	}
	if _, ok := c.servingExpired[key]; !ok && len(c.servingExpired) >= warmupServingCapacity {
		var oldestKey string
		var oldest time.Time
		for id, until := range c.servingExpired {
			if oldestKey == "" || until.Before(oldest) {
				oldestKey, oldest = id, until
			}
		}
		delete(c.servingExpired, oldestKey)
	}
	c.servingExpired[key] = now.Add(time.Hour)
}

func (c *SessionCache) recentServingExpiry(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.servingExpired[key]
	if ok && !time.Now().Before(until) {
		delete(c.servingExpired, key)
		return false
	}
	return ok
}

// The legacy Manager labels even a Claude-only model route as "mixed". Inspect
// the actual candidate providers; a genuinely heterogeneous route stays legacy.
func warmupServingClaudeRoute(provider string, available []*Auth) bool {
	if !strings.EqualFold(provider, "claude") && !strings.EqualFold(provider, "mixed") {
		return false
	}
	if len(available) == 0 {
		return false
	}
	for _, a := range available {
		if a == nil || !strings.EqualFold(a.Provider, "claude") {
			return false
		}
	}
	return true
}

func hasWarmupForkMarker(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, "<fork-boilerplate>")
	case []any:
		for _, item := range v {
			if hasWarmupForkMarker(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if hasWarmupForkMarker(item) {
				return true
			}
		}
	}
	return false
}

func hasWarmupCacheControl(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if hasWarmupCacheControl(item) {
				return true
			}
		}
	case map[string]any:
		if _, ok := v["cache_control"]; ok {
			return true
		}
		for _, item := range v {
			if hasWarmupCacheControl(item) {
				return true
			}
		}
	}
	return false
}

// Request summaries contain hashes and sizes only, never prompts or credentials.
// Unknown/opaque content can preserve affinity but cannot authorize migration.
type warmupRequestSummary struct {
	system, tools, first    [32]byte
	messages, inputCost     int
	known, singleUser, fork bool
	identityKnown           bool
	task                    [32]byte
	taskBytes               int
	uncachedTail            bool
	cacheTTL                time.Duration
}

type warmupServingSession struct {
	reserved, child, parentAffine bool
	protected                     bool
	source                        string
	revision                      uint64
	assignedAt, lastSeen          time.Time
	summary                       warmupRequestSummary
}

type warmupServingAccount struct {
	count                              int
	selectionOrder                     uint64
	unchangedSince, lastAssigned, seen time.Time
}

type warmupMigrationCharge struct {
	at     time.Time
	tokens int
}

// clearWarmupServing removes child bindings and opt-in metadata while preserving
// ordinary root bindings. Re-enabling cannot revive a former reservation pin.
func (s *AdaptiveSelector) clearWarmupServing() {
	s.servingMu.Lock()
	defer s.servingMu.Unlock()
	s.clearWarmupServingLocked()
}

func (s *AdaptiveSelector) clearWarmupServingLocked() {
	s.servingEpoch++
	s.servingAccounts = nil
	s.servingCharges = nil
	s.servingOrder = 0
	s.servingActive.Store(false)
	if s.cache != nil {
		s.cache.mu.Lock()
		defer s.cache.mu.Unlock()
		for key, entry := range s.cache.entries {
			if entry.serving != nil && entry.serving.child {
				s.cache.rememberServingExpiryLocked(key, entry)
				delete(s.cache.entries, key)
			} else if entry.serving != nil {
				entry.serving = nil
				s.cache.entries[key] = entry
			}
		}
	}
}

// servingEntry does not renew TTL: lastSeen must describe the previous request
// until the migration decision has been made. The underlying cache continues to
// own expiry, invalidation and cleanup for both legacy and opt-in bindings.
func (s *AdaptiveSelector) servingEntry(key string, now time.Time) (sessionEntry, bool) {
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	entry, ok := s.cache.entries[key]
	if !ok {
		return sessionEntry{}, false
	}
	if time.Now().After(entry.expiresAt) || (entry.serving != nil && now.Sub(entry.serving.lastSeen) > s.cache.ttl) {
		s.cache.rememberServingExpiryLocked(key, entry)
		delete(s.cache.entries, key)
		return sessionEntry{}, false
	}
	return entry, true
}

func (s *AdaptiveSelector) setServingEntry(key, authID string, state warmupServingSession, now time.Time) uint64 {
	state.lastSeen = now
	if state.assignedAt.IsZero() {
		state.assignedAt = now
	}
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	previous, exists := s.cache.entries[key]
	if state.revision == 0 || !exists || previous.serving == nil || previous.authID != authID || previous.serving.revision != state.revision {
		s.servingRevision++
		state.revision = s.servingRevision
	}
	if _, exists := s.cache.entries[key]; !exists && len(s.cache.entries) >= warmupServingCapacity {
		oldestKey := ""
		var oldest time.Time
		for k, entry := range s.cache.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = k, entry.expiresAt
			}
		}
		s.cache.rememberServingExpiryLocked(oldestKey, s.cache.entries[oldestKey])
		delete(s.cache.entries, oldestKey)
	}
	delete(s.cache.servingExpired, key)
	s.cache.entries[key] = sessionEntry{authID: authID, expiresAt: time.Now().Add(s.cache.ttl), serving: &state}
	return state.revision
}

func warmupChildKey(rootKey, agentID string) string {
	hash := sha256.Sum256([]byte(rootKey + "\x00" + agentID))
	return "warmup-child::" + hex.EncodeToString(hash[:])
}

func (s *AdaptiveSelector) pickWithWarmupServing(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) (*Auth, bool, error) {
	s.servingMu.Lock()
	defer s.servingMu.Unlock()
	// A Pick may have waited behind a disable while holding an older snapshot.
	// Re-read under the same lock that clears pins and migration reservations.
	cfg = s.scheduling()
	if cfg.WarmupServingReserve <= 0 {
		s.clearWarmupServingLocked()
		return nil, false, nil
	}
	s.servingActive.Store(true)
	s.observeServingAccounts(available, cfg, now)
	retry := failoverMatureOnlyFromMetadata(opts.Metadata)
	agentID := sessionHeaderValue(opts.Headers, "X-Claude-Code-Agent-Id")
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	headerRoot := sessionHeaderValue(opts.Headers, "X-Claude-Code-Session-Id")
	if headerRoot != "" && (primaryID == "" || strings.HasPrefix(primaryID, "msg:")) {
		primaryID, fallbackID = "claude:"+headerRoot, ""
	}
	identityConflict := headerRoot != "" && primaryID != "claude:"+headerRoot
	if !s.sessionAffinity || s.cache == nil || primaryID == "" {
		matureOnly, suppressReserve, _ := warmupSelectionPolicy(ctx)
		if matureOnly {
			picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, true), cfg, now)
			if !ok {
				return nil, true, newWarmupBusyError()
			}
			return picked, true, nil
		}
		if !suppressReserve && !retry && agentID == "" {
			if picked, ok := s.reserveWarmingPick(ctx, available, cfg, now, true, ""); ok {
				s.logPick(ctx, "reserve-warming-new", provider, model, "", picked, cfg, now)
				return picked, true, nil
			}
		}
		return nil, false, nil
	}

	key := provider + "::" + primaryID + "::" + model
	parentKey := key
	child := agentID != ""
	if child {
		key = warmupChildKey(key, agentID)
		if parentID := sessionHeaderValue(opts.Headers, "X-Claude-Code-Parent-Agent-Id"); parentID != "" {
			parentKey = warmupChildKey(parentKey, parentID)
		}
	}
	summary := summarizeWarmupRequest(opts.OriginalRequest)
	entry, bound := s.servingEntry(key, now)
	bindingKey := key
	matureOnly, suppressReserve, proof := warmupSelectionPolicy(ctx)
	staleProof := proof != nil && (proof.selector != s || proof.epoch != s.servingEpoch || proof.key != key || !bound || entry.serving == nil || proof.revision != entry.serving.revision || proof.authID != entry.authID)
	if staleProof {
		// Re-read current state after every wait; never copy back the old proof.
		suppressReserve = true
	}
	// An alias may inherit an existing root binding, but never a parent's child
	// identity. Such inheritance is not a fresh reserve opportunity.
	if !bound && !child && fallbackID != "" && fallbackID != primaryID {
		fallbackKey := provider + "::" + fallbackID + "::" + model
		entry, bound = s.servingEntry(fallbackKey, now)
		if bound {
			// Alias overlap shares the existing owner's ticket, while a normal
			// successful alias selection still commits only its own new key.
			bindingKey = fallbackKey
		}
	}
	borrow, borrowProof := warmupBorrowPolicy(ctx)
	if !borrow && bound && entry.serving != nil && entry.serving.protected && !retry {
		for _, candidate := range available {
			if candidate == nil || candidate.ID != entry.authID || s.isMature(candidate, cfg, now) || s.overWarmupBudget(candidate, cfg, now) {
				continue
			}
			sameWait := borrowProof != nil && borrowProof.selector == s && borrowProof.key == bindingKey && borrowProof.authID == entry.authID && borrowProof.revision == entry.serving.revision && borrowProof.epoch == s.servingEpoch
			borrow = sameWait || s.gate.InFlight(candidate.ID) > 0
			break
		}
	}
	if borrow {
		// The original request owns this binding. Borrowed attempts must not
		// renew its TTL, replace its summary, or commit a retry's selected auth.
		warmupMarkBorrow(ctx)
		picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, true), cfg, now)
		if !ok {
			return nil, true, newWarmupBusyError()
		}
		s.logPick(ctx, "warmup-concurrent-borrow-mature", provider, model, primaryID, picked, cfg, now)
		return picked, true, nil
	}
	newOpportunity := !bound && !suppressReserve && !s.cache.recentServingExpiry(key)
	if !child && !summary.singleUser {
		newOpportunity = false
	}
	if !bound && !child && fallbackID != "" {
		newOpportunity = newOpportunity && !s.cache.recentServingExpiry(provider+"::"+fallbackID+"::"+model)
	}
	state := warmupServingSession{child: child, assignedAt: now, summary: summary}
	if bound && entry.serving != nil {
		state = *entry.serving
	}
	reasonPrefix := ""
	if child && !bound {
		parent, parentOK := s.servingEntry(parentKey, now)
		fresh := !identityConflict && parentOK && parent.serving != nil && isFreshWarmupChild(summary, parent.serving.summary) && !warmupInheritedTaskPrefix(opts.OriginalRequest, parent.serving.summary)
		state.parentAffine = !fresh
		if fresh {
			reasonPrefix = "child-fresh/"
		} else {
			reasonPrefix = "child-parent-affine/"
			if parentOK {
				entry, bound = parent, true
				if parent.serving != nil {
					state.reserved = parent.serving.reserved
					state.protected = parent.serving.protected
					if state.protected {
						state.source = "inherited"
					}
				}
			}
		}
	}
	var picked *Auth
	var reason string
	var err error
	if matureOnly && staleProof && bound && entry.serving != nil {
		for _, candidate := range available {
			if candidate == nil || candidate.ID != entry.authID || s.overWarmupBudget(candidate, cfg, now) || !s.hasConcurrencyHeadroom(candidate, cfg, now) || s.pendingWarmupBudgetBusy(candidate, cfg, now) {
				continue
			}
			rpm, burst := s.rateLimitParams(candidate, cfg, now)
			allowed := warmupRateChargeAvailable(ctx, candidate.ID)
			if !allowed {
				allowed, _ = s.limiter.AllowOrDelay(candidate.ID, rpm, burst)
			}
			if allowed {
				picked, reason = candidate, "warmup-wait-current-binding"
				warmupRecordRateCharge(ctx, candidate.ID, true)
			}
			break
		}
	}
	if matureOnly && picked == nil {
		picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, true), cfg, now)
		if !ok {
			return nil, true, newWarmupBusyError()
		}
		state.reserved, state.protected, state.source = false, false, ""
		if !bound || picked.ID != entry.authID {
			state.assignedAt = now
		}
		state.summary = summary
		revision := s.setServingEntry(key, picked.ID, state, now)
		warmupRecordBinding(ctx, s, key, picked.ID, revision, s.servingEpoch, false)
		s.logPick(ctx, "warmup-wait-handoff-mature", provider, model, primaryID, picked, cfg, now)
		return picked, true, nil
	}
	if picked == nil && bound && !retry {
		var boundAuth *Auth
		for _, a := range available {
			if a != nil && a.ID == entry.authID {
				boundAuth = a
				break
			}
		}
		if boundAuth != nil && entry.serving != nil && !state.parentAffine {
			picked, reason = s.tryWarmupMigration(ctx, boundAuth, available, state, summary, cfg, now)
			if picked != nil {
				state.reserved, state.assignedAt = true, now
				state.protected, state.source = true, "migration"
			}
		}
		if picked == nil && boundAuth != nil && state.protected && !s.isMature(boundAuth, cfg, now) && !s.overWarmupBudget(boundAuth, cfg, now) {
			delay := 100 * time.Millisecond
			if s.hasConcurrencyHeadroom(boundAuth, cfg, now) && !s.pendingWarmupBudgetBusy(boundAuth, cfg, now) {
				rpm, burst := s.rateLimitParams(boundAuth, cfg, now)
				allowed, next := warmupRateChargeAvailable(ctx, boundAuth.ID), time.Duration(0)
				if !allowed {
					allowed, next = s.limiter.AllowOrDelay(boundAuth.ID, rpm, burst)
				}
				if allowed {
					picked, reason = boundAuth, "sticky-keep-warming-protected"
					warmupRecordRateCharge(ctx, picked.ID, true)
				} else {
					delay = next
				}
			}
			if picked == nil {
				return nil, true, &warmupSelectionWait{selector: s, key: bindingKey, authID: boundAuth.ID, revision: state.revision, epoch: s.servingEpoch, delay: delay}
			}
		}
		if picked == nil {
			// Failed keeps are ordinary reselection, never new reserve draws.
			picked, reason, err = s.resolveSticky(ctx, provider, model, opts, auths, available, cfg, now, key, entry.authID)
			state.reserved, state.protected, state.source = false, false, ""
		}
	} else if picked == nil && newOpportunity && !retry && !state.parentAffine {
		if chosen, ok := s.reserveWarmingPick(ctx, available, cfg, now, true, ""); ok {
			picked, reason, state.reserved = chosen, "reserve-warming-new", true
			state.protected, state.source = true, "reserve"
		}
	}
	if picked == nil && err == nil {
		picked, reason, err = s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, key)
		state.reserved = false
		state.protected, state.source = false, ""
		if err == nil && picked != nil && newOpportunity && !retry && !state.parentAffine && !s.isMature(picked, cfg, now) && len(s.scoreCandidates(available, cfg, now, true)) > 0 {
			state.protected, state.source = true, "weighted"
		}
	}
	if err == nil && picked != nil {
		if state.protected {
			reason += "/protected-" + state.source
		}
		if !bound || picked.ID != entry.authID {
			state.assignedAt = now
		}
		state.summary = summary
		revision := s.setServingEntry(key, picked.ID, state, now)
		warmupRecordBinding(ctx, s, key, picked.ID, revision, s.servingEpoch, state.protected)
		s.logPick(ctx, reasonPrefix+reason, provider, model, primaryID, picked, cfg, now)
	}
	return picked, true, err
}

// Counts reflect real outbound attempts, not successful service. Selection time
// is tracked separately so concurrent fresh sessions do not all target the same
// as-yet uncounted account. No selection or failure stamps a production anchor.
func (s *AdaptiveSelector) observeServingAccounts(available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) {
	if s.servingAccounts == nil {
		s.servingAccounts = make(map[string]warmupServingAccount)
	}
	seen := make(map[string]bool)
	for _, a := range available {
		if a == nil || !s.adaptiveEligible(a, cfg) || s.isMature(a, cfg, now) || s.overWarmupBudget(a, cfg, now) || !s.hasConcurrencyHeadroom(a, cfg, now) {
			continue
		}
		seen[a.ID] = true
		state, ok := s.servingAccounts[a.ID]
		if !ok && len(s.servingAccounts) >= warmupServingCapacity {
			continue
		}
		count := s.gate.DailyCount(a.ID)
		if !ok || state.count != count {
			state.unchangedSince = now
		}
		state.count, state.seen = count, now
		s.servingAccounts[a.ID] = state
	}
	for id := range s.servingAccounts {
		if !seen[id] {
			delete(s.servingAccounts, id)
		}
	}
}

// The candidate floor only chooses among warming accounts; the outer lottery
// governs new opportunities. Existing request/concurrency/health gates and the
// original weighted limiter (including its soft overflow) remain authoritative.
func (s *AdaptiveSelector) reserveWarmingPick(ctx context.Context, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, lottery bool, exclude string) (*Auth, bool) {
	if cfg.WarmupServingReserve <= 0 || len(s.scoreCandidates(available, cfg, now, true)) == 0 {
		return nil, false
	}
	pool := s.scoreCandidates(available, cfg, now, false)
	filtered := make([]adaptiveCandidate, 0, len(pool))
	var best warmupServingAccount
	for _, c := range pool {
		if !strings.EqualFold(c.auth.Provider, "claude") || c.auth.ID == exclude || s.isMature(c.auth, cfg, now) || !s.hasConcurrencyHeadroom(c.auth, cfg, now) {
			continue
		}
		state, observed := s.servingAccounts[c.auth.ID]
		if !observed {
			continue
		}
		if !lottery && (now.Sub(state.unchangedSince) < warmupObservationPeriod(cfg) || (!state.lastAssigned.IsZero() && now.Sub(state.lastAssigned) < warmupObservationPeriod(cfg))) {
			continue
		}
		if len(filtered) == 0 || state.count < best.count || (state.count == best.count && state.selectionOrder < best.selectionOrder) {
			filtered, best = filtered[:0], state
		}
		if state.count == best.count && state.selectionOrder == best.selectionOrder {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) == 0 || (lottery && s.rng() >= cfg.WarmupServingReserve) {
		return nil, false
	}
	picked, ok := s.pickFromCandidatesForRequest(ctx, filtered, cfg, now)
	if ok {
		state := s.servingAccounts[picked.ID]
		s.servingOrder++
		state.lastAssigned, state.selectionOrder = now, s.servingOrder
		s.servingAccounts[picked.ID] = state
	}
	return picked, ok
}

func warmupObservationPeriod(cfg internalconfig.AccountSchedulingConfig) time.Duration {
	if cfg.WarmupServingMaxBindingAgeSeconds > 0 {
		// Saturate before multiplication to avoid overflow from a valid large int.
		if cfg.WarmupServingMaxBindingAgeSeconds > int((24*time.Hour)/time.Second) {
			return 24 * time.Hour
		}
		return time.Duration(cfg.WarmupServingMaxBindingAgeSeconds) * time.Second
	}
	return time.Hour
}

func (s *AdaptiveSelector) tryWarmupMigration(ctx context.Context, bound *Auth, available []*Auth, state warmupServingSession, next warmupRequestSummary, cfg internalconfig.AccountSchedulingConfig, now time.Time) (*Auth, string) {
	if cfg.WarmupServingMigrationTokenBudget <= 0 || next.uncachedTail || !next.known || !state.summary.known || state.assignedAt.IsZero() {
		return nil, ""
	}
	// A newly reserved segment must first get a stable service period.
	if state.reserved && now.Sub(state.assignedAt) < warmupObservationPeriod(cfg) {
		return nil, ""
	}
	reason := ""
	if now.Sub(state.lastSeen) >= state.summary.cacheTTL {
		reason = "migration-idle"
	} else if next.messages < state.summary.messages && next.inputCost <= state.summary.inputCost/2 && next.first != state.summary.first {
		reason = "migration-context-reset"
	} else if cfg.WarmupServingMaxBindingAgeSeconds > 0 && now.Sub(state.assignedAt).Seconds() >= float64(cfg.WarmupServingMaxBindingAgeSeconds) {
		reason = "migration-age"
	}
	if reason == "" {
		return nil, ""
	}
	if s.gate.InFlight(bound.ID) != 0 {
		selectorLogEntry(ctx).Debug("adaptive-select: migration-skip-inflight")
		return nil, ""
	}
	kept := s.servingCharges[:0]
	used := 0
	for _, charge := range s.servingCharges {
		if now.Sub(charge.at) < time.Hour {
			kept = append(kept, charge)
			used += charge.tokens
		}
	}
	s.servingCharges = kept
	if len(kept) >= warmupServingCapacity || next.inputCost > cfg.WarmupServingMigrationTokenBudget-used {
		selectorLogEntry(ctx).Debug("adaptive-select: migration-skip-budget")
		return nil, ""
	}
	// This reservation and selection share servingMu with every opted-in Pick.
	// No other request can spend the budget or move the binding in between.
	s.servingCharges = append(s.servingCharges, warmupMigrationCharge{at: now, tokens: next.inputCost})
	picked, ok := s.reserveWarmingPick(ctx, available, cfg, now, false, bound.ID)
	if !ok {
		s.servingCharges = s.servingCharges[:len(s.servingCharges)-1]
		selectorLogEntry(ctx).Debug("adaptive-select: migration-skip-no-target")
		return nil, ""
	}
	selectorLogEntry(ctx).Debugf("adaptive-select: %s estimated-input-tokens=%d", reason, next.inputCost)
	return picked, reason
}

func isFreshWarmupChild(child, parent warmupRequestSummary) bool {
	return child.identityKnown && parent.identityKnown && !child.fork && child.singleUser && child.first != parent.first && child.system != parent.system && child.taskBytes > 0 && parent.taskBytes > 0 && child.task != parent.task
}

func summarizeWarmupRequest(payload []byte) warmupRequestSummary {
	summary := warmupRequestSummary{cacheTTL: time.Hour}
	if len(payload) == 0 || len(payload) > 4*1024*1024 {
		return summary
	}
	var request map[string]any
	if json.Unmarshal(payload, &request) != nil {
		return summary
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) == 0 {
		return summary
	}
	system := warmupSystemText(request["system"])
	if system == "" {
		return summary
	}
	summary.system = sha256.Sum256([]byte(system))
	tools, _ := json.Marshal(request["tools"])
	summary.tools = sha256.Sum256(tools)
	first, _ := json.Marshal(withoutWarmupCacheControl(messages[0]))
	summary.first = sha256.Sum256(first)
	summary.messages = len(messages)
	summary.singleUser = warmupFreshUserMessage(messages[0]) && (len(messages) == 1 || warmupAuxiliaryMessages(messages[1:]))
	if task := warmupCanonicalTask(messages[0]); task != "" {
		summary.task, summary.taskBytes = sha256.Sum256([]byte(task)), len(task)
	}
	// Inspect identity by message/content position, not type fields in tool input.
	// Keep the previous conservative migration-cost gate independently unchanged.
	summary.identityKnown = warmupIdentityMessages(messages)
	// Byte count plus framing overhead is deliberately conservative for text;
	// media, opaque blocks and unrecognized content cannot use this estimate.
	summary.known = warmupTextOnly(messages)
	summary.inputCost = len(payload) + 1024 + 32*len(messages)
	summary.fork = hasWarmupForkMarker(messages)
	if len(messages) > 1 {
		previous, _ := messages[len(messages)-2].(map[string]any)
		last, _ := messages[len(messages)-1].(map[string]any)
		// The observed CLI compact request retains a cached assistant prefix
		// followed by an uncached user task. Protect this broader structure,
		// without guessing compact from natural-language instructions.
		summary.uncachedTail = previous["role"] == "assistant" && last["role"] == "user" && hasWarmupCacheControl(previous) && !hasWarmupCacheControl(last)
	}
	seenCache, unknownTTL, longest := false, false, time.Duration(0)
	var scan func(any)
	scan = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				scan(item)
			}
		case map[string]any:
			if raw, exists := v["cache_control"]; exists {
				seenCache = true
				cc, _ := raw.(map[string]any)
				ttl, _ := cc["ttl"].(string)
				duration := time.Duration(0)
				switch ttl {
				case "", "5m":
					duration = 5 * time.Minute
				case "1h":
					duration = time.Hour
				default:
					unknownTTL = true
				}
				if cc["type"] != "ephemeral" {
					unknownTTL = true
				}
				if duration > longest {
					longest = duration
				}
			}
			for _, item := range v {
				scan(item)
			}
		}
	}
	scan(request)
	if seenCache && !unknownTTL {
		summary.cacheTTL = longest
	}
	return summary
}

func warmupSystemText(value any) string {
	var blocks []string
	switch v := value.(type) {
	case string:
		blocks = append(blocks, v)
	case []any:
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				return ""
			}
			text, _ := part["text"].(string)
			blocks = append(blocks, text)
		}
	default:
		return ""
	}
	clean := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if strings.HasPrefix(block, "x-anthropic-billing-header:") {
			continue
		}
		if index := strings.Index(block, "\n\ngitStatus: This is the git status at the start of the conversation."); index >= 0 {
			block = block[:index]
		}
		if strings.TrimSpace(block) != "" {
			clean = append(clean, block)
		}
	}
	return strings.Join(clean, "\n\n")
}

func withoutWarmupCacheControl(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutWarmupCacheControl(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if k != "cache_control" {
				out[k] = withoutWarmupCacheControl(item)
			}
		}
		return out
	default:
		return value
	}
}

// Only a text user task can prove freshness; a lone tool_result may be resumed history.
func warmupFreshUserMessage(value any) bool {
	message, ok := value.(map[string]any)
	if !ok || message["role"] != "user" || !warmupIdentityContent(message["content"], false) {
		return false
	}
	if text, ok := message["content"].(string); ok {
		return strings.TrimSpace(text) != ""
	}
	blocks, _ := message["content"].([]any)
	if len(blocks) == 0 {
		return false
	}
	for _, raw := range blocks {
		if block, _ := raw.(map[string]any); block["type"] != "text" || !warmupStringField(block, "text", true) {
			return false
		}
	}
	return true
}

// Recognize only the observed trailing date notice, not arbitrary system context.
// The original request and existing hash inputs remain unchanged;
// message counts, input cost and cache TTL retain their existing accounting.
func warmupDateMessage(value any) bool {
	message, ok := value.(map[string]any)
	if !ok || !warmupOnlyKeys(message, "role", "content") || message["role"] != "system" {
		return false
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 || !warmupIdentityContent(content, false) {
		return false
	}
	block, _ := content[0].(map[string]any)
	text, ok := block["text"].(string)
	const prefix = "Today's date is "
	if !ok || len(text) != len(prefix)+len("2006-01-02.") || !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, ".") {
		return false
	}
	_, err := time.Parse("2006-01-02", text[len(prefix):len(text)-1])
	return err == nil
}

func warmupIdentityMessages(messages []any) bool {
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || !warmupOnlyKeys(message, "role", "content") {
			return false
		}
		switch message["role"] {
		case "user", "assistant":
			if !warmupIdentityContent(message["content"], false) {
				return false
			}
		case "system":
			// Text CLI notices remain history, regardless of their position.
			// singleUser separately rejects them as fresh-child evidence.
			if !warmupIdentityText(message["content"]) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func warmupIdentityText(value any) bool {
	if !warmupIdentityContent(value, false) {
		return false
	}
	if blocks, ok := value.([]any); ok {
		for _, raw := range blocks {
			if raw.(map[string]any)["type"] != "text" {
				return false
			}
		}
	}
	return true
}

// caller is metadata and input is ordinary JSON; only content arrays hold blocks.
// tool_reference is recognized only inside tool_result; unknown/media stays opaque.
func warmupIdentityContent(value any, toolResult bool) bool {
	if _, ok := value.(string); ok {
		return true
	}
	blocks, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || !warmupIdentityCacheControl(block) {
			return false
		}
		switch block["type"] {
		case "text":
			if !warmupOnlyKeys(block, "type", "text", "cache_control") || !warmupStringField(block, "text", false) {
				return false
			}
		case "thinking":
			if toolResult || !warmupOnlyKeys(block, "type", "thinking", "signature", "cache_control") || !warmupStringField(block, "thinking", false) {
				return false
			}
			if _, exists := block["signature"]; exists && !warmupStringField(block, "signature", false) {
				return false
			}
		case "tool_use":
			if toolResult || !warmupOnlyKeys(block, "type", "id", "name", "input", "caller", "cache_control") || !warmupStringField(block, "id", true) || !warmupStringField(block, "name", true) {
				return false
			}
			if _, ok := block["input"].(map[string]any); !ok {
				return false
			}
			if rawCaller, exists := block["caller"]; exists {
				caller, ok := rawCaller.(map[string]any)
				if !ok || len(caller) != 1 || caller["type"] != "direct" {
					return false
				}
			}
		case "tool_result":
			if toolResult || !warmupOnlyKeys(block, "type", "tool_use_id", "content", "is_error", "cache_control") || !warmupStringField(block, "tool_use_id", true) || !warmupIdentityContent(block["content"], true) {
				return false
			}
			if rawError, exists := block["is_error"]; exists {
				if _, ok := rawError.(bool); !ok {
					return false
				}
			}
		case "tool_reference":
			if !toolResult || !warmupOnlyKeys(block, "type", "tool_name") || !warmupStringField(block, "tool_name", true) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func warmupIdentityCacheControl(block map[string]any) bool {
	raw, exists := block["cache_control"]
	if !exists {
		return true
	}
	cc, ok := raw.(map[string]any)
	if !ok || !warmupOnlyKeys(cc, "type", "ttl") || cc["type"] != "ephemeral" {
		return false
	}
	if ttl, exists := cc["ttl"]; exists && ttl != "5m" && ttl != "1h" {
		return false
	}
	return true
}

func warmupOnlyKeys(value map[string]any, keys ...string) bool {
	for key := range value {
		found := false
		for _, allowed := range keys {
			if key == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func warmupStringField(value map[string]any, key string, nonempty bool) bool {
	text, ok := value[key].(string)
	return ok && (!nonempty || text != "")
}

func warmupTextOnly(value any) bool {
	switch v := value.(type) {
	case string, nil, bool, float64:
		return true
	case []any:
		for _, item := range v {
			if !warmupTextOnly(item) {
				return false
			}
		}
		return true
	case map[string]any:
		if raw, exists := v["type"]; exists {
			t, _ := raw.(string)
			switch t {
			case "text", "tool_use", "tool_result", "thinking", "ephemeral":
			default:
				return false
			}
		}
		for _, item := range v {
			if !warmupTextOnly(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
