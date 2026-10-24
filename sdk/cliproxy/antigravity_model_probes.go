package cliproxy

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// antigravityModelRefreshRequest captures publication ownership at admission.
// A queued successor must not adopt a later epoch from an external publisher.
type antigravityModelRefreshRequest struct {
	ctx           context.Context
	auth          *coreauth.Auth
	provider      string
	registryEpoch uint64
}

type antigravityHintPersistRequest struct {
	ctx      context.Context
	auth     *coreauth.Auth
	routeKey string
	endpoint string
	hints    antigravityModelCapabilityHints
	cancel   context.CancelFunc
}

// queueAntigravityModelRefresh coalesces registration and periodic probes into
// one worker per account. Repeated updates replace a single queued successor,
// rather than accumulating singleflight followers while an upstream is stalled.
func (s *Service) queueAntigravityModelRefresh(ctx context.Context, auth *coreauth.Auth, provider string) {
	if s == nil || !s.beginAntigravityCallback() {
		return
	}
	defer s.antigravityProbeWg.Done()

	if s == nil || auth == nil || auth.ID == "" || auth.Disabled || s.antigravityHomeEnabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || pluginHostHasAuthModelProvider(s.pluginHost, auth.Provider) {
		return
	}
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	if lifetime != nil && lifetime.Err() != nil {
		return
	}
	request := &antigravityModelRefreshRequest{
		ctx: ctx, auth: auth.Clone(), provider: provider,
		registryEpoch: GlobalModelRegistry().ClientRegistrationEpoch(auth.ID),
	}
	token, _ := request.auth.Metadata["access_token"].(string)
	probe := &antigravityAccountProbe{
		auth: request.auth, routeKey: s.antigravityCapabilityKey(request.auth), token: strings.TrimSpace(token),
	}
	s.antigravityProbeMu.Lock()
	// Late stale callers must not replace the latest queued auth snapshot.
	if s.antigravityProbesStopped || ctx.Err() != nil || !s.antigravityAccountProbeCurrent(probe) {
		s.antigravityProbeMu.Unlock()
		return
	}
	if s.antigravityRefreshPending == nil {
		s.antigravityRefreshPending = make(map[string]*antigravityModelRefreshRequest)
	}
	queued, running := s.antigravityRefreshPending[auth.ID]
	if queued != nil && queued.registryEpoch > request.registryEpoch {
		// Concurrent registration callers can reach admission out of order.
		s.antigravityProbeMu.Unlock()
		return
	}
	s.antigravityRefreshPending[auth.ID] = request
	if !running {
		s.antigravityProbeWg.Add(1)
	}
	s.antigravityProbeMu.Unlock()
	if !running {
		go s.runAntigravityAccountRefresh(auth.ID)
	}
}

func (s *Service) runAntigravityAccountRefresh(authID string) {
	defer s.antigravityProbeWg.Done()
	defer s.signalAntigravityModelRefresh()
	for {
		s.antigravityProbeMu.Lock()
		request := s.antigravityRefreshPending[authID]
		if request == nil {
			delete(s.antigravityRefreshPending, authID)
			s.antigravityProbeMu.Unlock()
			return
		}
		s.antigravityRefreshPending[authID] = nil
		s.antigravityProbeMu.Unlock()
		s.executeAntigravityModelRefresh(request)
	}
}

func (s *Service) executeAntigravityModelRefresh(request *antigravityModelRefreshRequest) {
	if request.ctx.Err() != nil || pluginHostHasAuthModelProvider(s.pluginHost, request.auth.Provider) {
		return
	}
	probeCtx, cancel := context.WithCancel(request.ctx)
	defer cancel()
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	if lifetime != nil {
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		if lifetime.Err() != nil {
			return
		}
	}
	expectedKey, hints := s.probeAndRegisterAntigravityModels(probeCtx, request.auth, request.provider, request.auth.RegistrationEpoch, request.registryEpoch)
	if hints.ModelIDs != nil {
		s.queueAntigravityHintPersistence(request.ctx, request.auth, expectedKey, antigravityModelBaseURLs(request.auth)[0], hints)
	}
}

// antigravityAccountProbe owns both queued and connected work for one auth snapshot.
// Cancellation releases network slots without imposing an upstream timeout.
type antigravityAccountProbe struct {
	auth      *coreauth.Auth
	routeKey  string
	flightKey string
	token     string
	cancel    context.CancelFunc
}

func (s *Service) beginAntigravityAccountProbe(parent context.Context, auth *coreauth.Auth, routeKey, flightKey, token string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	var stop func() bool
	if lifetime != nil {
		stop = context.AfterFunc(lifetime, cancel)
		if lifetime.Err() != nil {
			cancel()
		}
	}
	var changes <-chan struct{}
	var unsubscribe func()
	if s.coreManager != nil {
		// Subscribe before validation, so a native refresh/removal cannot fall
		// between checking the auth and installing the invalidation observer.
		changes, unsubscribe = s.coreManager.WatchAuthChanges(auth.ID)
	}
	var watcherDone chan struct{}
	probe := &antigravityAccountProbe{auth: auth.Clone(), routeKey: routeKey, flightKey: flightKey, token: token, cancel: cancel}
	s.antigravityProbeMu.Lock()
	valid := !s.antigravityProbesStopped && ctx.Err() == nil && s.antigravityAccountProbeCurrent(probe)
	if valid {
		if s.antigravityAccountProbes == nil {
			s.antigravityAccountProbes = make(map[string]map[*antigravityAccountProbe]struct{})
		}
		if s.antigravityAccountProbes[auth.ID] == nil {
			s.antigravityAccountProbes[auth.ID] = make(map[*antigravityAccountProbe]struct{})
		}
		s.antigravityAccountProbes[auth.ID][probe] = struct{}{}
	}
	s.antigravityProbeMu.Unlock()
	if valid && changes != nil {
		watcherDone = make(chan struct{})
		go func() {
			defer close(watcherDone)
			for {
				select {
				case <-ctx.Done():
					return
				case _, open := <-changes:
					if !open {
						return
					}
					s.cancelStaleAntigravityProbes(auth.ID)
				}
			}
		}()
	}
	cleanup := func() {
		cancel()
		if unsubscribe != nil {
			unsubscribe()
		}
		if watcherDone != nil {
			<-watcherDone
		}
		if stop != nil {
			stop()
		}
		s.antigravityProbeMu.Lock()
		delete(s.antigravityAccountProbes[auth.ID], probe)
		if len(s.antigravityAccountProbes[auth.ID]) == 0 {
			delete(s.antigravityAccountProbes, auth.ID)
		}
		s.antigravityProbeMu.Unlock()
		s.signalAntigravityModelRefresh()
	}
	if !valid {
		cleanup()
		return ctx, func() {}, false
	}
	return ctx, cleanup, true
}

// Caller holds antigravityProbeMu, making tracking and invalidation indivisible.
func (s *Service) antigravityAccountProbeCurrent(probe *antigravityAccountProbe) bool {
	if s.antigravityHomeEnabled() {
		return false
	}
	current := probe.auth
	if s.coreManager != nil {
		var exists bool
		current, exists = s.coreManager.GetByID(probe.auth.ID)
		if !exists || current == nil {
			return false
		}
	}
	token, _ := current.Metadata["access_token"].(string)
	return !current.Disabled && current.Status != coreauth.StatusDisabled &&
		antigravityAuthModelSettingsEqual(current, probe.auth) &&
		s.antigravityCapabilityKey(current) == probe.routeKey && strings.TrimSpace(token) == probe.token
}

// Run after the manager/config mutation. New probes check current state while
// joining this same map, so deletion cannot slip between validation and tracking.
// Empty authID invalidates stale probes across all accounts after config changes.
func (s *Service) queueAntigravityHintPersistence(ctx context.Context, auth *coreauth.Auth, expectedKey, endpoint string, hints antigravityModelCapabilityHints) {
	if s == nil || !s.beginAntigravityCallback() {
		return
	}
	defer s.antigravityProbeWg.Done()

	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" || auth.Disabled || expectedKey == "" || hints.ModelIDs == nil || hints.revision == 0 || s.antigravityHomeEnabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || pluginHostHasAuthModelProvider(s.pluginHost, auth.Provider) {
		return
	}
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	if lifetime != nil && lifetime.Err() != nil {
		return
	}
	antigravityCapabilityMu.RLock()
	entry := antigravityCapabilityCache[expectedKey]
	stale := entry.hints.ModelIDs == nil || entry.hints.revision != hints.revision || entry.persistedRevision >= hints.revision
	antigravityCapabilityMu.RUnlock()
	if stale {
		return
	}
	request := &antigravityHintPersistRequest{
		ctx:      ctx,
		auth:     auth.Clone(),
		routeKey: expectedKey,
		endpoint: endpoint,
		hints:    hints.clone(),
	}
	s.antigravityProbeMu.Lock()
	if s.antigravityProbesStopped || ctx.Err() != nil || !s.antigravityHintPersistCurrent(request) {
		s.antigravityProbeMu.Unlock()
		return
	}
	if s.antigravityPersistPending == nil {
		s.antigravityPersistPending = make(map[string]*antigravityHintPersistRequest)
	}
	queued, running := s.antigravityPersistPending[auth.ID]
	if queued != nil && queued.hints.revision > request.hints.revision {
		s.antigravityProbeMu.Unlock()
		return
	}
	if active := s.antigravityPersistActive[auth.ID]; active != nil && (active.hints.revision < request.hints.revision || active.routeKey != request.routeKey) {
		if active.cancel != nil {
			active.cancel()
		}
	}
	s.antigravityPersistPending[auth.ID] = request
	if !running {
		s.antigravityProbeWg.Add(1)
	}
	s.antigravityProbeMu.Unlock()
	if !running {
		go s.runAntigravityHintPersistence(auth.ID)
	}
}

func (s *Service) runAntigravityHintPersistence(authID string) {
	defer s.antigravityProbeWg.Done()
	defer s.signalAntigravityModelRefresh()
	for {
		s.antigravityProbeMu.Lock()
		request := s.antigravityPersistPending[authID]
		if request == nil {
			delete(s.antigravityPersistPending, authID)
			delete(s.antigravityPersistActive, authID)
			s.antigravityProbeMu.Unlock()
			return
		}
		s.antigravityPersistPending[authID] = nil
		persistCtx, cancel := context.WithCancel(request.ctx)
		request.cancel = cancel
		if s.antigravityPersistActive == nil {
			s.antigravityPersistActive = make(map[string]*antigravityHintPersistRequest)
		}
		s.antigravityPersistActive[authID] = request
		s.antigravityProbeMu.Unlock()
		s.executeAntigravityHintPersistence(persistCtx, cancel, request)
	}
}

func (s *Service) executeAntigravityHintPersistence(persistCtx context.Context, cancel context.CancelFunc, request *antigravityHintPersistRequest) {
	defer cancel()
	if persistCtx.Err() != nil || pluginHostHasAuthModelProvider(s.pluginHost, request.auth.Provider) {
		return
	}
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	if lifetime != nil {
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		if lifetime.Err() != nil {
			return
		}
	}
	if s.persistAntigravityCapabilityHints(persistCtx, request.auth, request.routeKey, request.endpoint, request.hints) && request.ctx.Err() == nil {
		s.queueAntigravityHintPersistence(request.ctx, request.auth, request.routeKey, request.endpoint, request.hints)
	}
}

func (s *Service) antigravityHintPersistCurrent(request *antigravityHintPersistRequest) bool {
	if request == nil || request.auth == nil || s.antigravityHomeEnabled() {
		return false
	}
	current := request.auth
	if s.coreManager != nil {
		var exists bool
		current, exists = s.coreManager.GetByID(request.auth.ID)
		if !exists || current == nil {
			return false
		}
	}
	return !current.Disabled && current.Status != coreauth.StatusDisabled &&
		current.Provider == request.auth.Provider &&
		current.RegistrationEpoch == request.auth.RegistrationEpoch &&
		s.antigravityCapabilityKey(current) == request.routeKey
}

func (s *Service) cancelStaleAntigravityProbes(authID string) {
	if s == nil {
		return
	}
	s.antigravityProbeMu.Lock()
	for id, probes := range s.antigravityAccountProbes {
		if authID != "" && id != authID {
			continue
		}
		for probe := range probes {
			if !s.antigravityAccountProbeCurrent(probe) {
				probe.cancel()
				// An updated snapshot with the same token may start immediately instead
				// of joining an obsolete leader that is still unwinding its HTTP request.
				antigravityCapabilityGroup.Forget(probe.flightKey)
				delete(probes, probe)
			}
		}
		if len(probes) == 0 {
			delete(s.antigravityAccountProbes, id)
		}
	}
	for id, active := range s.antigravityPersistActive {
		if authID != "" && id != authID {
			continue
		}
		if active != nil && !s.antigravityHintPersistCurrent(active) {
			if active.cancel != nil {
				active.cancel()
			}
		}
	}
	for id, pending := range s.antigravityPersistPending {
		if authID != "" && id != authID {
			continue
		}
		if pending != nil && !s.antigravityHintPersistCurrent(pending) {
			s.antigravityPersistPending[id] = nil
		}
	}
	s.antigravityProbeMu.Unlock()
	s.signalAntigravityModelRefresh()
}

func (s *Service) antigravityModelRefreshWake() chan struct{} {
	s.antigravityProbeMu.Lock()
	defer s.antigravityProbeMu.Unlock()
	if s.antigravityRefreshWake == nil {
		s.antigravityRefreshWake = make(chan struct{}, 1)
	}
	return s.antigravityRefreshWake
}

func (s *Service) signalAntigravityModelRefresh() {
	if s == nil {
		return
	}
	select {
	case s.antigravityModelRefreshWake() <- struct{}{}:
	default:
	}
}

// A reusable idle barrier permits concurrent admission while a caller waits.
// Shutdown first closes admission; ordinary Wait callers only drain current work.
// Unlike WaitGroup, a new generation cannot race a zero-count waiter wakeup.
type antigravityProbeBarrier struct {
	mu    sync.Mutex
	count int
	idle  chan struct{}
}

func (b *antigravityProbeBarrier) Add(delta int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count+delta < 0 {
		panic("negative Antigravity probe count")
	}
	if b.count == 0 && delta > 0 {
		b.idle = make(chan struct{})
	}
	b.count += delta
	if b.count == 0 && b.idle != nil {
		close(b.idle)
		b.idle = nil
	}
}
func (b *antigravityProbeBarrier) Done() { b.Add(-1) }
func (b *antigravityProbeBarrier) Wait() {
	for {
		b.mu.Lock()
		idle := b.idle
		b.mu.Unlock()
		if idle == nil {
			return
		}
		<-idle
	}
}

// Reserve ownership before DoChan can launch its closure. A cancelled follower
// abandons its unused reservation immediately. A leader owns its reservation
// until its real HTTP work and auth-change watcher have both unwound.
type antigravityProbeTicket struct {
	owner  *Service
	cancel context.CancelFunc
	state  atomic.Uint32 // 0 pending, 1 claimed by leader, 2 abandoned before start
	once   sync.Once
}

func (s *Service) reserveAntigravityProbe(parent context.Context) (context.Context, *antigravityProbeTicket) {
	ctx, cancel := context.WithCancel(parent)
	ticket := &antigravityProbeTicket{owner: s, cancel: cancel}
	s.antigravityProbeMu.Lock()
	if s.antigravityProbesStopped {
		s.antigravityProbeMu.Unlock()
		cancel()
		return ctx, nil
	}
	if s.antigravityProbeTickets == nil {
		s.antigravityProbeTickets = make(map[*antigravityProbeTicket]struct{})
	}
	s.antigravityProbeTickets[ticket] = struct{}{}
	s.antigravityProbeWg.Add(1)
	s.antigravityProbeMu.Unlock()
	return ctx, ticket
}

// start claims a reserved leader before it can read shared probe dependencies.
func (t *antigravityProbeTicket) start(ctx context.Context) (bool, error) {
	if !t.state.CompareAndSwap(0, 1) {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		t.finish()
		return false, err
	}
	return true, nil
}

func (t *antigravityProbeTicket) finish() {
	t.once.Do(func() {
		t.owner.antigravityProbeMu.Lock()
		delete(t.owner.antigravityProbeTickets, t)
		t.owner.antigravityProbeMu.Unlock()
		t.owner.antigravityProbeWg.Done()
	})
}
func (t *antigravityProbeTicket) abandon() {
	t.cancel()
	if t.state.CompareAndSwap(0, 2) {
		t.finish()
	}
}

// StopAntigravityProbes closes this Service's admission and cancels only its
// owned work. WaitAntigravityProbes provides the separate completion barrier.
func (s *Service) StopAntigravityProbes() {
	if s == nil {
		return
	}
	s.antigravityProbeMu.Lock()
	s.antigravityProbesStopped = true
	var cancels []context.CancelFunc
	for ticket := range s.antigravityProbeTickets {
		cancels = append(cancels, ticket.cancel)
	}
	for _, probes := range s.antigravityAccountProbes {
		for probe := range probes {
			cancels = append(cancels, probe.cancel)
		}
	}
	for id := range s.antigravityRefreshPending {
		s.antigravityRefreshPending[id] = nil
	}
	for id := range s.antigravityPersistPending {
		s.antigravityPersistPending[id] = nil
	}
	for _, active := range s.antigravityPersistActive {
		if active.cancel != nil {
			cancels = append(cancels, active.cancel)
		}
	}
	s.antigravityProbeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// 准入和计数必须在任何UA/global依赖读取之前建立，Stop后不再读共享依赖。
func (s *Service) beginAntigravityCallback() bool {
	s.antigravityProbeMu.Lock()
	defer s.antigravityProbeMu.Unlock()
	if s.antigravityProbesStopped {
		return false
	}
	s.antigravityProbeWg.Add(1)
	return true
}
func (b *antigravityProbeBarrier) WaitContext(ctx context.Context) error {
	for {
		b.mu.Lock()
		idle := b.idle
		b.mu.Unlock()
		if idle == nil {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// 仅成功返回才允许释放UA、临时文件或其他共享依赖。
func (s *Service) quiesceAntigravity(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.StopAntigravityProbes()
	if s.coreManager != nil {
		s.coreManager.StopAutoRefresh()
		if err := s.coreManager.ShutdownAutoRefresh(ctx); err != nil {
			return err
		}
	}
	return s.antigravityProbeWg.WaitContext(ctx)
}
