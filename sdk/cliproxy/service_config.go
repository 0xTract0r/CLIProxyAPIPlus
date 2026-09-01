package cliproxy

import (
	"context"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

func (s *Service) applyConfigUpdate(newCfg *config.Config) {
	s.applyConfigUpdateWithAuthSynthesis(context.Background(), newCfg, true)
}

func (s *Service) applyWatcherConfigUpdate(newCfg *config.Config) {
	s.applyConfigUpdateWithAuthSynthesis(context.Background(), newCfg, false)
}

type configCommit struct {
	cfg      *config.Config
	sequence uint64
}

type routingRuntimeState struct {
	strategy           string
	sessionAffinity    bool
	sessionAffinityTTL time.Duration

	// accountScheduling is the AccountSchedulingConfig snapshot captured for
	// this commit (openspec/changes/add-adaptive-account-scheduling). It only
	// matters when strategy == internalconfig.RoutingStrategyAdaptive, and is
	// deliberately EXCLUDED from selectorShapeEqual below: a change to it
	// alone (e.g. a tier-weight or warm-up-curve tweak) must never trigger a
	// selector rebuild -- newRoutingSelectorWithLiveScheduling's live
	// accessor picks such a change up on the very next Pick instead. This
	// field also means routingRuntimeState is no longer comparable with
	// == / != as a whole (AccountSchedulingConfig embeds a slice,
	// WarmupCurve) -- selectorShapeEqual is the only supported comparison.
	accountScheduling internalconfig.AccountSchedulingConfig
}

// selectorShapeEqual reports whether a and b would produce the same *kind* of
// Selector (same base strategy, same session-affinity wiring) -- i.e. whether
// SetSelector actually needs to rebuild the selector. See the
// accountScheduling field doc for why that field is intentionally excluded
// from this comparison.
func (a routingRuntimeState) selectorShapeEqual(b routingRuntimeState) bool {
	return a.strategy == b.strategy &&
		a.sessionAffinity == b.sessionAffinity &&
		a.sessionAffinityTTL == b.sessionAffinityTTL
}

func normalizedRoutingRuntimeState(cfg *config.Config) routingRuntimeState {
	state := routingRuntimeState{
		strategy:           "round-robin",
		sessionAffinityTTL: time.Hour,
		accountScheduling:  internalconfig.DefaultAccountSchedulingConfig(),
	}
	if cfg == nil {
		return state
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy)) {
	case "fill-first", "fillfirst", "ff":
		state.strategy = "fill-first"
	case internalconfig.RoutingStrategyAdaptive:
		// openspec/changes/add-adaptive-account-scheduling: opt into the
		// tier/quota/warm-up-aware selector. newRoutingSelectorWithLiveScheduling
		// below is what actually wires this to coreauth.NewAdaptiveSelector.
		state.strategy = internalconfig.RoutingStrategyAdaptive
	}
	// fork: honor both the legacy ClaudeCodeSessionAffinity flag and the new
	// universal SessionAffinity so existing configs keep session affinity on.
	state.sessionAffinity = cfg.Routing.ClaudeCodeSessionAffinity || cfg.Routing.SessionAffinity
	if ttl := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); ttl != "" {
		if parsed, errParse := time.ParseDuration(ttl); errParse == nil && parsed > 0 {
			state.sessionAffinityTTL = parsed
		}
	}
	// internal/config/account_scheduling.go's DefaultAccountSchedulingConfig
	// is already merged into cfg.AccountScheduling by every config-load path
	// (config_load.go / parse.go) before YAML unmarshal, so this is always a
	// fully-defaulted snapshot, never a caller-constructed zero value.
	state.accountScheduling = cfg.AccountScheduling
	return state
}

// newRoutingSelector builds a Selector from state alone, scoring the
// "adaptive" strategy against state.accountScheduling as a fixed snapshot
// (no live re-read). This is what the pre-Service construction path
// (builder.go, which has no *Service yet to read a live config from) calls;
// the config-reload path (applyManagerConfig, below) calls
// newRoutingSelectorWithLiveScheduling instead so an adaptive selector
// re-reads AccountSchedulingConfig on every Pick without ever needing to be
// rebuilt.
func newRoutingSelector(state routingRuntimeState) coreauth.Selector {
	return newRoutingSelectorWithLiveScheduling(state, nil)
}

// newRoutingSelectorWithLiveScheduling is newRoutingSelector plus an optional
// live AccountSchedulingConfig accessor for the "adaptive" strategy
// (openspec/changes/add-adaptive-account-scheduling, tasks.md 1.2/5.1). When
// live is non-nil it is wired via coreauth.WithAdaptiveSchedulingProvider so a
// config-file edit to warm-up-curve / tier-weight / mature-limit knobs takes
// effect on the very next Pick without a selector rebuild -- rebuilding would
// otherwise drop in-flight session-stickiness bindings and reset per-account
// token buckets (coreauth.AdaptiveSelector self-hosts both). When live is
// nil, the adaptive selector still scores against state.accountScheduling --
// just as a fixed snapshot instead of a live read.
//
// Every other strategy value (including the pre-existing "round-robin" /
// "fill-first" behavior) is completely unchanged by this function -- the
// "adaptive" branch returns before reaching the pre-existing
// session-affinity-wrapping code below it.
func newRoutingSelectorWithLiveScheduling(state routingRuntimeState, live func() internalconfig.AccountSchedulingConfig) coreauth.Selector {
	var selector coreauth.Selector
	if state.strategy == "fill-first" {
		selector = &coreauth.FillFirstSelector{}
	} else {
		selector = &coreauth.RoundRobinSelector{}
	}
	if state.strategy == internalconfig.RoutingStrategyAdaptive {
		var opts []coreauth.AdaptiveSelectorOption
		if live != nil {
			opts = append(opts, coreauth.WithAdaptiveSchedulingProvider(live))
		}
		// coreauth.AdaptiveSelector self-hosts session affinity with
		// design.md D5 maturity grading via its own internal SessionCache --
		// it must NOT additionally be wrapped in NewSessionAffinitySelectorWithConfig,
		// which would short-circuit on a cache hit and never let the adaptive
		// selector see/grade the binding (see adaptive_selector.go's type
		// doc). So this branch returns directly, skipping the
		// session-affinity wrap below entirely.
		return coreauth.NewAdaptiveSelector(coreauth.AdaptiveSelectorConfig{
			Fallback:        selector,
			Scheduling:      state.accountScheduling,
			SessionAffinity: state.sessionAffinity,
			SessionTTL:      state.sessionAffinityTTL,
		}, opts...)
	}
	if state.sessionAffinity {
		selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback: selector,
			TTL:      state.sessionAffinityTTL,
		})
	}
	return selector
}

// liveAccountSchedulingConfig reads the current AccountSchedulingConfig off
// the live *Service config (s.cfg, protected by s.cfgMu -- the same lock
// commitConfigUpdate uses to swap s.cfg on every config commit/hot-reload).
// Passed to the adaptive selector via coreauth.WithAdaptiveSchedulingProvider
// (see newRoutingSelectorWithLiveScheduling) so a config-file edit to the
// account-scheduling section is picked up on the very next Pick, even on a
// reload that does not otherwise change routing.strategy / session-affinity
// (and therefore does not trigger a selector rebuild -- see
// routingRuntimeState.selectorShapeEqual).
func (s *Service) liveAccountSchedulingConfig() internalconfig.AccountSchedulingConfig {
	if s == nil {
		return internalconfig.DefaultAccountSchedulingConfig()
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil {
		return internalconfig.DefaultAccountSchedulingConfig()
	}
	return cfg.AccountScheduling
}

func (s *Service) applyConfigUpdateWithAuthSynthesis(ctx context.Context, newCfg *config.Config, synthesizeConfigAuths bool) bool {
	commit := s.commitConfigUpdate(newCfg)
	if commit.cfg == nil {
		return false
	}
	return s.applyConfigRuntime(ctx, commit, synthesizeConfigAuths)
}

// commitConfigUpdate applies only in-memory configuration state. Runtime work that
// may block on plugins, models, storage, or networking is deliberately deferred.
func (s *Service) commitConfigUpdate(newCfg *config.Config) configCommit {
	if s == nil {
		return configCommit{}
	}

	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()

	if newCfg == nil {
		s.cfgMu.RLock()
		newCfg = s.cfg
		s.cfgMu.RUnlock()
	}
	if newCfg == nil {
		return configCommit{}
	}

	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()
	s.configSequence++
	return configCommit{cfg: newCfg, sequence: s.configSequence}
}

func (s *Service) configCommitCurrent(commit configCommit) bool {
	if s == nil || commit.sequence == 0 {
		return false
	}
	s.configUpdateMu.Lock()
	current := s.configSequence == commit.sequence
	s.configUpdateMu.Unlock()
	return current
}

func (s *Service) applyConfigRuntime(ctx context.Context, commit configCommit, synthesizeConfigAuths bool) bool {
	cfg := commit.cfg
	if s == nil || cfg == nil {
		return false
	}
	s.configRuntimeMu.Lock()
	defer s.configRuntimeMu.Unlock()
	if !s.configCommitCurrent(commit) {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	if !s.applyManagerConfig(ctx, commit) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if !s.applyPprofConfigContext(ctx, cfg) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if !s.updateServerClientsContext(ctx, cfg) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	registrationCtx := coreauth.WithSkipPersist(ctx)
	s.syncPluginRuntimeConfigForConfig(registrationCtx, cfg)
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	var auths []*coreauth.Auth
	if s.coreManager != nil {
		auths = s.coreManager.List()
	}
	s.registerAvailableExecutors(registrationCtx, executorRegistrationOptions{
		includeBaseline:   cfg.Home.Enabled,
		forceReplaceAuths: true,
		auths:             auths,
	})
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if synthesizeConfigAuths {
		s.registerConfigAPIKeyAuths(registrationCtx, cfg)
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	if s.coreManager != nil && !cfg.Home.Enabled && cfg.SaveCooldownStatus {
		if errRestoreCooldown := s.coreManager.RestoreCooldownStates(registrationCtx); errRestoreCooldown != nil && ctx.Err() == nil {
			log.Warnf("failed to restore cooldown state after config update: %v", errRestoreCooldown)
		}
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	s.syncPluginModelRuntime(registrationCtx)
	return ctx.Err() == nil
}

func (s *Service) applyManagerConfig(ctx context.Context, commit configCommit) bool {
	if s == nil || s.coreManager == nil || commit.cfg == nil {
		return s != nil && commit.cfg != nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	routingState := normalizedRoutingRuntimeState(commit.cfg)
	if s.appliedRoutingState == nil || !s.appliedRoutingState.selectorShapeEqual(routingState) {
		s.coreManager.SetSelector(newRoutingSelectorWithLiveScheduling(routingState, s.liveAccountSchedulingConfig))
		s.appliedRoutingState = &routingState
	}
	s.applyRetryConfig(commit.cfg)
	store := s.resolveCooldownStateStore(commit.cfg)
	if !s.coreManager.ApplyConfigWithCooldownStateStore(ctx, commit.cfg, store) {
		return false
	}
	s.coreManager.SetOAuthModelAlias(commit.cfg.OAuthModelAlias)
	return true
}

func (s *Service) updateServerClientsContext(ctx context.Context, cfg *config.Config) bool {
	if s == nil || cfg == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if s.updateServerClientsContextFn != nil {
		return s.updateServerClientsContextFn(ctx, cfg)
	}
	if s.server == nil {
		return true
	}
	return s.server.UpdateClientsContext(ctx, cfg)
}

func (s *Service) reloadConfigFromWatcher() bool {
	if s == nil || s.watcher == nil {
		return false
	}
	return s.watcher.ReloadConfigIfChanged()
}

func (s *Service) registerConfigAPIKeyAuths(ctx context.Context, cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	configSynth := synthesizer.NewConfigSynthesizer()
	auths, errSynthesize := configSynth.Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		log.Warnf("failed to synthesize config API key auths: %v", errSynthesize)
		return
	}

	registrationCtx := coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)
	tasks := make([]modelRegistrationTask, 0, len(auths))
	needsAliasRebuild := false
	for _, auth := range auths {
		if !coreauth.IsConfigAPIKeyAuth(auth) {
			continue
		}
		prepared := s.prepareCoreAuthForModelRegistration(registrationCtx, auth)
		if prepared == nil {
			continue
		}
		needsAliasRebuild = true
		authForRegistration := prepared
		tasks = append(tasks, modelRegistrationTask{
			phase:    modelRegistrationPhaseConfigAPIKey,
			category: modelRegistrationCategory(authForRegistration),
			run: func(compatCache *openAICompatibilityRegistrationCache) {
				s.completeModelRegistrationForAuthWithCache(registrationCtx, authForRegistration, compatCache)
			},
		})
	}
	if needsAliasRebuild {
		s.coreManager.RefreshAPIKeyModelAlias()
	}
	s.runModelRegistrationTasks(registrationCtx, tasks)
}

func forceHomeRuntimeConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	cfg.APIKeys = nil
	cfg.UsageStatisticsEnabled = true
	cfg.DisableCooling = true
	cfg.SaveCooldownStatus = false
	cfg.WebsocketAuth = false
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.Plugins.StoreAuth = nil
}
