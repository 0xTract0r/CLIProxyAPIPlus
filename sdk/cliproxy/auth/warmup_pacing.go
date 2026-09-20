package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	warmupPacingSchema      = 1
	warmupPacingMaxAccounts = 256
	warmupPacingMaxAttempts = 16384
	warmupPacingMaxBindings = 256
	warmupPacingMaxBytes    = 8 << 20
)

// WarmupPacingStore must durably replace one account's complete sidecar before
// returning nil. Keys are SHA-256 digests, never credential or session text.
// Load must be read-only: management snapshots may call it without initializing
// the account, repairing storage, or changing the pacer's cache.
type WarmupPacingStore interface {
	Load(key string) ([]byte, error)
	Save(key string, data []byte) error
}

// WarmupPacingLimits must contain the account's effective, already scaled
// limits. Config changes must call Reconfigure at their effective event time.
type WarmupPacingLimits struct {
	Config        internalconfig.WarmupTrafficPacingConfig `json:"config"`
	DailyRequests int                                      `json:"daily_requests"`
	RPM           int                                      `json:"rpm"`
	Concurrency   int                                      `json:"concurrency"`
	DailyTokens   int64                                    `json:"daily_tokens"`
}

type WarmupPacingRequest struct {
	GroupID         string
	BindingID       string
	CountOnly       bool
	EstimatedTokens int64
	EstimateKnown   bool
}

// WarmupPacingDecision separates temporary RPM/concurrency waits from long-term
// budget/admission refusals. Callers retain the existing shared 30s wait budget.
type WarmupPacingDecision struct {
	Allowed                                             bool
	Reason                                              string
	RetryAfter                                          time.Duration
	Balance                                             float64
	MinuteRequests, DayRequests, InFlight, ActiveGroups int
	Tokens, PendingTokens                               int64
}

type WarmupPacingDeniedError struct{ Decision WarmupPacingDecision }

func (e *WarmupPacingDeniedError) Error() string { return "warmup pacing: " + e.Decision.Reason }

type pacingRecord struct {
	At            int64  `json:"at"`
	Tokens        int64  `json:"tokens"`
	Estimate      int64  `json:"estimate"`
	Group         string `json:"group,omitempty"`
	Binding       string `json:"binding,omitempty"`
	EstimateKnown bool   `json:"estimate_known"`
	CountOnly     bool   `json:"count_only"`
	UsageComplete bool   `json:"usage_complete"`
}

type pacingState struct {
	Schema  int                          `json:"schema"`
	Key     string                       `json:"key"`
	Last    int64                        `json:"last"`
	Balance float64                      `json:"balance"`
	Limits  WarmupPacingLimits           `json:"limits"`
	Next    uint64                       `json:"next"`
	Records map[uint64]pacingRecord      `json:"records"`
	Groups  map[string]map[string]int64  `json:"groups"`
	Legacy  map[int64]pacingLegacyBucket `json:"legacy,omitempty"`
}

type pacingAccount struct {
	state pacingState
	live  map[uint64]*WarmupPacingAttempt
	fault error
}

// WarmupPacer owns only pacing accounting, never auth snapshots or network I/O.
// A store failure poisons the account until reload, including ambiguous failures
// after rename: stale memory must never authorize a second spend or refund.
type WarmupPacer struct {
	mu       sync.Mutex
	store    WarmupPacingStore
	now      func() time.Time
	accounts map[string]*pacingAccount
}

// WarmupPacingAttempt is process-local. Recovery keeps its durable debit but
// never recreates a dead process's concurrency lease or refund capability.
type WarmupPacingAttempt struct {
	pacer                *WarmupPacer
	key                  string
	id                   uint64
	sent, done, released bool
}

func NewWarmupPacer(store WarmupPacingStore, now func() time.Time) *WarmupPacer {
	if now == nil {
		now = time.Now
	}
	return &WarmupPacer{store: store, now: now, accounts: make(map[string]*pacingAccount)}
}

func pacingDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (l WarmupPacingLimits) validate() error {
	if err := l.Config.Validate(); err != nil {
		return err
	}
	if l.Config.Enabled && (l.DailyRequests <= 0 || l.RPM <= 0 || l.Concurrency <= 0 || l.DailyTokens < 0) {
		return errors.New("warmup pacing requires positive effective request, RPM and concurrency limits and nonnegative token budget")
	}
	if l.Config.Enabled && (int64(l.Config.ActiveBindingIdleSeconds) > math.MaxInt64/int64(time.Second) || l.Config.MaxActiveBindings > warmupPacingMaxBindings) {
		return errors.New("warmup pacing binding configuration exceeds bounded state capacity")
	}
	return nil
}

func clonePacingState(s pacingState) pacingState {
	c := s
	c.Legacy = make(map[int64]pacingLegacyBucket, len(s.Legacy))
	for hour, bucket := range s.Legacy {
		c.Legacy[hour] = bucket
	}
	c.Records = make(map[uint64]pacingRecord, len(s.Records))
	for k, v := range s.Records {
		c.Records[k] = v
	}
	c.Groups = make(map[string]map[string]int64, len(s.Groups))
	for group, members := range s.Groups {
		c.Groups[group] = make(map[string]int64, len(members))
		for binding, at := range members {
			c.Groups[group][binding] = at
		}
	}
	return c
}

func (p *WarmupPacer) load(key string) (*pacingAccount, error) {
	if a := p.accounts[key]; a != nil {
		return a, a.fault
	}
	if p.store == nil {
		return nil, errors.New("warmup pacing store is unavailable")
	}
	if len(p.accounts) >= warmupPacingMaxAccounts {
		// Safe eviction reloads durable state; active or faulted accounts stay.
		for k, a := range p.accounts {
			if len(a.live) == 0 && a.fault == nil {
				delete(p.accounts, k)
				break
			}
		}
		if len(p.accounts) >= warmupPacingMaxAccounts {
			return nil, errors.New("warmup pacing account capacity reached")
		}
	}
	a := &pacingAccount{live: make(map[uint64]*WarmupPacingAttempt)}
	data, err := p.store.Load(key)
	if errors.Is(err, fs.ErrNotExist) {
		a.state = pacingState{Schema: warmupPacingSchema, Key: key, Records: make(map[uint64]pacingRecord), Groups: make(map[string]map[string]int64)}
	} else if err != nil {
		a.fault = fmt.Errorf("load warmup pacing state: %w", err)
	} else {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if len(data) > warmupPacingMaxBytes {
			a.fault = errors.New("warmup pacing sidecar exceeds size limit")
		} else if err := decoder.Decode(&a.state); err != nil {
			a.fault = fmt.Errorf("invalid warmup pacing state: %w", err)
		} else if decoder.Decode(new(any)) != io.EOF {
			a.fault = errors.New("trailing warmup pacing state data")
		} else {
			a.fault = validatePacingState(a.state, key)
		}
	}
	p.accounts[key] = a
	return a, a.fault
}

func validatePacingState(s pacingState, key string) error {
	if s.Schema != warmupPacingSchema || s.Key != key || s.Last <= 0 || math.IsNaN(s.Balance) || math.IsInf(s.Balance, 0) || s.Balance < 0 || s.Records == nil || s.Groups == nil || len(s.Records) > warmupPacingMaxAttempts || len(s.Groups) > warmupPacingMaxBindings {
		return errors.New("invalid warmup pacing schema or state bounds")
	}
	if err := validatePacingLegacy(s); err != nil {
		return err
	}
	storedLimits := s.Limits
	storedLimits.Config.Enabled = true
	if err := storedLimits.validate(); err != nil {
		return err
	}
	if s.Limits.DailyRequests <= 0 || s.Limits.Config.RequestBurst <= 0 || s.Balance > float64(s.Limits.Config.RequestBurst) {
		return errors.New("invalid warmup pacing stored balance")
	}
	members := 0
	for group, bindings := range s.Groups {
		if len(group) != 64 {
			return errors.New("invalid pacing group digest")
		}
		for binding, at := range bindings {
			members++
			if len(binding) != 64 || at <= 0 || at > s.Last {
				return errors.New("invalid pacing binding state")
			}
		}
	}
	if members > warmupPacingMaxBindings {
		return errors.New("warmup pacing member capacity exceeded")
	}
	for id, r := range s.Records {
		if id == 0 || id > s.Next || r.At <= 0 || r.At > s.Last || r.Tokens < 0 || r.Estimate < 0 || (r.Group == "") != (r.Binding == "") || (r.Group != "" && (len(r.Group) != 64 || len(r.Binding) != 64)) {
			return errors.New("invalid warmup pacing attempt")
		}
	}
	return nil
}

func (p *WarmupPacer) commit(a *pacingAccount, s pacingState) error {
	if err := validatePacingState(s, s.Key); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err == nil && len(data) > warmupPacingMaxBytes {
		err = errors.New("warmup pacing state capacity exceeded")
	}
	if err == nil {
		err = p.store.Save(s.Key, data)
	}
	if err != nil {
		a.fault = fmt.Errorf("persist warmup pacing state: %w", err)
		return a.fault
	}
	a.state = s
	return nil
}

func (p *WarmupPacer) advance(a *pacingAccount, s *pacingState, at int64) {
	if at < s.Last {
		at = s.Last
	}
	if s.Last > 0 {
		s.Balance = math.Min(pacingAvailableCapacity(a, s, 0), s.Balance+float64(at-s.Last)/float64(time.Second)*float64(s.Limits.DailyRequests)/86400)
	}
	s.Last = at
	for hour := range s.Legacy {
		if pacingLegacyExpiry(hour) <= at {
			delete(s.Legacy, hour)
		}
	}
	for id, r := range s.Records {
		if a.live[id] == nil && r.At <= at-int64(24*time.Hour) {
			delete(s.Records, id)
		}
	}
	idle := int64(time.Duration(s.Limits.Config.ActiveBindingIdleSeconds) * time.Second)
	for group, members := range s.Groups {
		for binding, seen := range members {
			if seen <= at-idle && !pacingLiveBinding(a, s, group, binding) {
				delete(members, binding)
			}
		}
		if len(members) == 0 {
			delete(s.Groups, group)
		}
	}
}

func pacingPendingSends(a *pacingAccount, exclude uint64) int {
	count := 0
	for id, lease := range a.live {
		if id != exclude && !lease.sent {
			count++
		}
	}
	return count
}

// Prepaid but unsent credits still occupy the bucket. Refilling their space
// before sending would let old reservations add a second burst on top of it.
func pacingAvailableCapacity(a *pacingAccount, s *pacingState, exclude uint64) float64 {
	return math.Max(0, float64(s.Limits.Config.RequestBurst-pacingPendingSends(a, exclude)))
}

func pacingCommittedGroup(a *pacingAccount, s *pacingState, group string) bool {
	idle := int64(time.Duration(s.Limits.Config.ActiveBindingIdleSeconds) * time.Second)
	for _, seen := range s.Groups[group] {
		if seen > s.Last-idle {
			return true
		}
	}
	for id, lease := range a.live {
		if !lease.sent || lease.released {
			continue
		}
		r := s.Records[id]
		if r.Group == group {
			if _, exists := s.Groups[group][r.Binding]; exists {
				return true
			}
		}
	}
	return false
}

func pacingLiveBinding(a *pacingAccount, s *pacingState, group, binding string) bool {
	for id := range a.live {
		r := s.Records[id]
		if !a.live[id].released && r.Group == group && r.Binding == binding {
			return true
		}
	}
	return false
}

func (p *WarmupPacer) prepare(account string, limits WarmupPacingLimits) (*pacingAccount, pacingState, error) {
	if account == "" {
		return nil, pacingState{}, errors.New("warmup pacing account is empty")
	}
	if err := limits.validate(); err != nil {
		return nil, pacingState{}, err
	}
	a, err := p.load(pacingDigest(account))
	if err != nil {
		return nil, pacingState{}, err
	}
	s := clonePacingState(a.state)
	at := p.now().UnixNano()
	if at <= 0 {
		return nil, s, errors.New("warmup pacing clock is invalid")
	}
	if s.Last == 0 {
		if !limits.Config.Enabled {
			return a, s, nil
		}
		s.Limits, s.Last = limits, at
		if err := p.commit(a, s); err != nil {
			return nil, s, err
		}
		return a, clonePacingState(s), nil
	}
	p.advance(a, &s, at)
	if s.Limits != limits {
		// A disabled zero config retains the last valid refill parameters.
		validated := limits
		validated.Config.Enabled = true
		if !limits.Config.Enabled && validated.validate() != nil {
			s.Limits.Config.Enabled = false
		} else {
			s.Limits = limits
		}
		s.Balance = math.Min(s.Balance, pacingAvailableCapacity(a, &s, 0))
		if err := p.commit(a, s); err != nil {
			return nil, s, err
		}
	}
	return a, s, nil
}

func pacingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

func pacingGroupKey(req WarmupPacingRequest) (string, string) {
	if req.CountOnly {
		return "", ""
	}
	return pacingDigest(req.GroupID), pacingDigest(req.BindingID)
}

func (p *WarmupPacer) decision(a *pacingAccount, s pacingState, req WarmupPacingRequest, exclude uint64) WarmupPacingDecision {
	d := WarmupPacingDecision{Balance: s.Balance, InFlight: len(a.live)}
	groups := make(map[string]bool, len(s.Groups))
	bindings := make(map[string]bool)
	for group, members := range s.Groups {
		groups[group] = true
		for binding := range members {
			bindings[group+binding] = true
		}
	}
	minuteExpiry, dayExpiry := int64(0), int64(0)
	unknownHistory := false
	for id, r := range s.Records {
		live := a.live[id] != nil
		pendingSend := live && !a.live[id].sent
		if live && !a.live[id].released && r.Group != "" {
			groups[r.Group] = true
			bindings[r.Group+r.Binding] = true
		}
		if id == exclude {
			d.InFlight--
			continue
		}
		if pendingSend || r.At > s.Last-int64(24*time.Hour) {
			d.DayRequests++
			if dayExpiry == 0 || r.At < dayExpiry {
				dayExpiry = r.At
			}
		}
		if live || r.At > s.Last-int64(24*time.Hour) {
			d.Tokens = pacingAdd(d.Tokens, r.Tokens)
			unknownHistory = unknownHistory || (!r.EstimateKnown && !r.UsageComplete)
		}
		if live {
			d.PendingTokens = pacingAdd(d.PendingTokens, r.Tokens)
		}
		if pendingSend || r.At > s.Last-int64(time.Minute) {
			d.MinuteRequests++
			if minuteExpiry == 0 || r.At < minuteExpiry {
				minuteExpiry = r.At
			}
		}
	}
	for hour, bucket := range s.Legacy {
		if pacingLegacyExpiry(hour) <= s.Last {
			continue
		}
		d.DayRequests = int(pacingAdd(int64(d.DayRequests), bucket.Requests))
		d.Tokens = pacingAdd(d.Tokens, bucket.Tokens)
		unknownHistory = unknownHistory || bucket.UnknownTokens
		at := pacingLegacyExpiry(hour) - int64(24*time.Hour)
		if bucket.Requests > 0 && (dayExpiry == 0 || at < dayExpiry) {
			dayExpiry = at
		}
	}
	d.ActiveGroups = len(groups)
	deny := func(reason string, delay time.Duration) WarmupPacingDecision {
		d.Reason = reason
		if delay > 0 {
			d.RetryAfter = delay
		}
		return d
	}
	if !s.Limits.Config.Enabled {
		d.Allowed, d.Reason = true, "disabled"
		return d
	}
	if req.EstimatedTokens < 0 {
		return deny("invalid-estimate", 0)
	}
	if !req.CountOnly && (req.GroupID == "" || req.BindingID == "") {
		return deny("unknown-group", 0)
	}
	if s.Limits.DailyTokens > 0 && !req.EstimateKnown {
		return deny("unknown-token-estimate", 0)
	}
	if s.Limits.DailyTokens > 0 && unknownHistory {
		return deny("unknown-token-history", 0)
	}
	if d.DayRequests >= s.Limits.DailyRequests {
		return deny("daily-budget", time.Duration(dayExpiry+int64(24*time.Hour)-s.Last))
	}
	if s.Limits.DailyTokens > 0 && (d.Tokens >= s.Limits.DailyTokens || req.EstimatedTokens > s.Limits.DailyTokens-d.Tokens) {
		return deny("token-budget", 0)
	}
	group, binding := pacingGroupKey(req)
	if exclude != 0 {
		r := s.Records[exclude]
		group, binding = r.Group, r.Binding
		if !req.CountOnly && d.ActiveGroups > s.Limits.Config.MaxActiveBindings {
			return deny("active-groups", 0)
		}
	}
	if !req.CountOnly && !bindings[group+binding] && len(bindings) >= warmupPacingMaxBindings {
		return deny("state-capacity", 0)
	}
	need := 1
	if !req.CountOnly && !groups[group] {
		if d.ActiveGroups >= s.Limits.Config.MaxActiveBindings {
			return deny("active-groups", 0)
		}
	}
	if !req.CountOnly && !pacingCommittedGroup(a, &s, group) {
		need = s.Limits.Config.MinAdmissionRequests
	}
	available := s.Balance
	if exclude != 0 {
		if pacingPendingSends(a, 0) > s.Limits.Config.RequestBurst {
			return deny("request-balance", 0)
		}
		// Recheck admission against this attempt's prepaid credit as well as
		// free balance. Another pending member cannot waive first admission.
		available++
	}
	if available+1e-9 < float64(need) {
		return deny("request-balance", time.Duration(math.Ceil((float64(need)-available)*86400/float64(s.Limits.DailyRequests)*float64(time.Second))))
	}
	if d.InFlight >= s.Limits.Concurrency {
		return deny("concurrency", 100*time.Millisecond)
	}
	if d.MinuteRequests >= s.Limits.RPM {
		return deny("rpm", time.Duration(minuteExpiry+int64(time.Minute)-s.Last))
	}
	if len(s.Records) >= warmupPacingMaxAttempts && exclude == 0 {
		return deny("state-capacity", 0)
	}
	d.Allowed, d.Reason = true, "ready"
	return d
}

// Peek may initialize the zero-balance anchor or persist changed limits, but it
// never reserves a group, consumes a request, or renews binding activity.
func (p *WarmupPacer) Peek(account string, limits WarmupPacingLimits, req WarmupPacingRequest) (WarmupPacingDecision, error) {
	if !limits.Config.Enabled {
		return WarmupPacingDecision{Allowed: true, Reason: "disabled"}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, s, err := p.prepare(account, limits)
	if err != nil {
		return WarmupPacingDecision{Reason: "state-error"}, err
	}
	return p.decision(a, s, req, 0), nil
}

// PeekConfigured cannot reconfigure or initialize policy. Selectors use this
// advisory read after the Manager publishes the latest effective limits.
func (p *WarmupPacer) PeekConfigured(account string, req WarmupPacingRequest) (WarmupPacingDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if account == "" {
		return WarmupPacingDecision{Reason: "state-error"}, errors.New("warmup pacing account is empty")
	}
	a, err := p.load(pacingDigest(account))
	if err != nil {
		return WarmupPacingDecision{Reason: "state-error"}, err
	}
	if a.state.Last == 0 {
		return WarmupPacingDecision{Reason: "state-error"}, errors.New("warmup pacing policy is not configured")
	}
	s := clonePacingState(a.state)
	p.advance(a, &s, p.now().UnixNano())
	return p.decision(a, s, req, 0), nil
}

func (p *WarmupPacer) Reconfigure(account string, limits WarmupPacingLimits) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _, err := p.prepare(account, limits)
	return err
}

func (p *WarmupPacer) Reserve(account string, limits WarmupPacingLimits, req WarmupPacingRequest) (*WarmupPacingAttempt, WarmupPacingDecision, error) {
	if !limits.Config.Enabled {
		return nil, WarmupPacingDecision{Allowed: true, Reason: "disabled"}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, s, err := p.prepare(account, limits)
	if err != nil {
		return nil, WarmupPacingDecision{Reason: "state-error"}, err
	}
	d := p.decision(a, s, req, 0)
	if !d.Allowed {
		return nil, d, nil
	}
	if s.Next == math.MaxUint64 {
		return nil, WarmupPacingDecision{Reason: "state-capacity"}, errors.New("warmup pacing attempt IDs exhausted")
	}
	s.Next++
	group, binding := pacingGroupKey(req)
	tokens := int64(0)
	if req.EstimateKnown {
		tokens = req.EstimatedTokens
	}
	s.Records[s.Next] = pacingRecord{At: s.Last, Tokens: tokens, Estimate: tokens, Group: group, Binding: binding, EstimateKnown: req.EstimateKnown, CountOnly: req.CountOnly}
	s.Balance = math.Max(0, s.Balance-1)
	if err := p.commit(a, s); err != nil {
		return nil, WarmupPacingDecision{Reason: "state-error"}, err
	}
	lease := &WarmupPacingAttempt{pacer: p, key: s.Key, id: s.Next}
	a.live[lease.id] = lease
	return lease, d, nil
}

// MarkSent is the linearization point immediately before an application HTTP
// attempt, including connection failures. No network work belongs in this call.
func (p *WarmupPacer) MarkSent(lease *WarmupPacingAttempt) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.attemptAccount(lease)
	if err != nil || lease == nil {
		return err
	}
	if lease.done || lease.sent {
		return errors.New("warmup pacing attempt already started or finished")
	}
	if lease.released {
		return &WarmupPacingDeniedError{Decision: WarmupPacingDecision{Reason: "binding-released"}}
	}
	s := clonePacingState(a.state)
	p.advance(a, &s, p.now().UnixNano())
	r := s.Records[lease.id]
	// Reservations can outlive preparation or a config update. Recheck current
	// hard limits without spending the same balance/concurrency lease twice.
	d := p.decision(a, s, WarmupPacingRequest{GroupID: r.Group, BindingID: r.Binding, CountOnly: r.CountOnly, EstimateKnown: r.EstimateKnown, EstimatedTokens: r.Tokens}, lease.id)
	if !d.Allowed {
		return &WarmupPacingDeniedError{Decision: d}
	}
	r.At = s.Last
	s.Records[lease.id] = r
	if r.Group != "" {
		if s.Groups[r.Group] == nil {
			s.Groups[r.Group] = make(map[string]int64)
		}
		s.Groups[r.Group][r.Binding] = s.Last
	}
	if err := p.commit(a, s); err != nil {
		return err
	}
	lease.sent = true
	return nil
}

func (p *WarmupPacer) attemptAccount(lease *WarmupPacingAttempt) (*pacingAccount, error) {
	if lease == nil {
		return nil, nil
	}
	if lease.pacer != p {
		return nil, errors.New("foreign warmup pacing attempt")
	}
	a := p.accounts[lease.key]
	if a == nil {
		if lease.done {
			return nil, nil
		}
		return nil, errors.New("missing warmup pacing attempt")
	}
	if a.fault != nil {
		return nil, a.fault
	}
	if !lease.done && a.live[lease.id] != lease {
		return nil, errors.New("invalid warmup pacing attempt")
	}
	return a, nil
}

// CancelUnsent refunds once only in the reserving process, never after MarkSent.
func (p *WarmupPacer) CancelUnsent(lease *WarmupPacingAttempt) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.attemptAccount(lease)
	if err != nil || lease == nil || lease.done || lease.sent {
		return err
	}
	s := clonePacingState(a.state)
	p.advance(a, &s, p.now().UnixNano())
	delete(s.Records, lease.id)
	s.Balance = math.Min(pacingAvailableCapacity(a, &s, lease.id), s.Balance+1)
	if err := p.commit(a, s); err != nil {
		return err
	}
	delete(a.live, lease.id)
	lease.done = true
	return nil
}

// Finish accepts scheduler-normalized tokens, not provider usage JSON. Only a
// complete terminal usage may reduce the estimate; partial/unknown usage keeps
// max(estimate, observed) until that attempt's original rolling window expires.
func (p *WarmupPacer) Finish(lease *WarmupPacingAttempt, complete bool, tokens int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.attemptAccount(lease)
	if err != nil || lease == nil || lease.done {
		return err
	}
	if !lease.sent {
		return errors.New("warmup pacing attempt was not marked sent")
	}
	if tokens < 0 {
		return errors.New("negative warmup pacing token usage")
	}
	s := clonePacingState(a.state)
	p.advance(a, &s, p.now().UnixNano())
	r := s.Records[lease.id]
	r.Tokens = tokens
	r.UsageComplete = complete
	if !complete && r.Tokens < r.Estimate {
		r.Tokens = r.Estimate
	}
	s.Records[lease.id] = r
	if !lease.released && r.Group != "" {
		if _, exists := s.Groups[r.Group][r.Binding]; exists {
			s.Groups[r.Group][r.Binding] = s.Last
		}
	}
	if err := p.commit(a, s); err != nil {
		return err
	}
	delete(a.live, lease.id)
	lease.done = true
	return nil
}

// ReleaseBinding removes only one member. Other parent/fork/alias members and
// active attempts keep their group's admission slot until their own release.
func (p *WarmupPacer) ReleaseBinding(account, group, binding string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.load(pacingDigest(account))
	if err != nil {
		return err
	}
	if a.state.Last == 0 {
		return nil
	}
	s := clonePacingState(a.state)
	p.advance(a, &s, p.now().UnixNano())
	g := pacingDigest(group)
	delete(s.Groups[g], pacingDigest(binding))
	if len(s.Groups[g]) == 0 {
		delete(s.Groups, g)
	}
	if err := p.commit(a, s); err != nil {
		return err
	}
	for id, lease := range a.live {
		r := s.Records[id]
		if r.Group == g && r.Binding == pacingDigest(binding) {
			lease.released = true
		}
	}
	return nil
}
