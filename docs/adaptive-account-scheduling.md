# Adaptive Account Scheduling

> Implements `openspec/changes/add-adaptive-account-scheduling`. Config field definitions
> are already fully covered in the `account-scheduling` section of
> `core/config.example.yaml` and are not repeated here; this document only adds the
> management API read-only field reference and operational notes. Fields/behavior have
> been verified against the current code as of 2026-09.

## 1. Overview

Setting `routing.strategy` to `adaptive` enables this account-selection strategy (the
default remains `round-robin`; when `adaptive` is not set, the `account-scheduling`
section has no effect and is ignored entirely). Once enabled, a single account selection
is jointly determined by the following layers of mechanism:

- **Weighted selection by subscription-tier capacity x live quota headroom x
  freshness**: the core formula is
  `weight = tier base capacity weight x (1 - quota utilization) x freshness factor`
  (see `AccountSelectionWeight` in `sdk/cliproxy/auth/account_weight.go`).
  - Tier base capacity weight is configured per provider separately: Claude distinguishes
    four tiers, `max_20x` / `max_5x` / `pro` / `unknown`; Codex distinguishes three tiers,
    `pro` / `plus` / `unknown`. The two weight sets are only ever compared within their own
    provider and never across providers (a Claude weight and a Codex weight are not
    comparable).
  - Quota headroom is taken from the account's "tightest" quota window (the window with
    the highest utilization), not from an arbitrary window or an average — an account
    whose `five_hour` window is already at 90% will not be treated as safely available
    just because its `seven_day` window still has headroom. When the quota snapshot is
    entirely unreadable, it is not treated as "0% used"; instead a neutral,
    conservatively-biased fallback value is used.
  - The freshness factor is derived from the account's age since `first_production_at`
    (see section 3.2), looked up against the stages of
    `account-scheduling.warmup-curve`: accounts still inside the curve get a factor
    `< 1`, and mature accounts that have passed the entire curve get a factor of `1`.
- **Per-account rate-limit smoothing (not a global pool)**: each account has its own
  independent token bucket (`sdk/cliproxy/auth/account_rate_limiter.go`), whose
  rate/burst capacity is taken from the `rpm-limit` / concurrency ceiling of whichever
  warm-up stage (or mature state) the account is currently in. One client hammering a
  single account does not squeeze the quota of other accounts in the pool. When a
  candidate account is momentarily rate-limited, the request is not rejected outright;
  weighted sampling simply continues over the remaining candidate pool ("burst traffic
  naturally routes to mature accounts" falls out of weight + token bucket acting
  together, with no separate burst-detection logic required).
- **Progressive ramp-up during a new account's warm-up period**: new accounts have
  `daily-budget` / `rpm-limit` / `concurrency-limit` raised week by week according to the
  stages configured in `warmup-curve` (5 stages by default, see
  `config.example.yaml`); once past the last stage (age >= 60 days by default) the
  account enters `mature-limits`, is no longer bound by a fixed daily budget, and is
  instead driven by quota headroom.
- **Tiered session stickiness**: when `routing.session-affinity: true`, this selector
  carries its own session-stickiness cache and applies tiered handling based on whether
  the account is mature (unlike the outer generic session-affinity wrapper, which simply
  "returns immediately on a cache hit, never consulting the inner selector"):
  - Sticky target is a **non-Claude/Codex account** (a provider this scheduler does not
    score): no tiering applied; behavior is identical to existing session-affinity.
  - Sticky target is **mature and has not hit its soft ceiling** (token bucket still
    permits it): stickiness is kept, preserving prompt-cache continuity.
  - Sticky target is **mature but has already hit its soft ceiling** (treated as
    "approaching the hard risk-control threshold"): stickiness is broken; a fresh
    weighted selection is made across the whole pool.
  - Sticky target is **still in its warm-up period, and a usable mature account exists in
    the pool**: stickiness is broken and the request is instead routed to a mature
    account (with rebinding, so subsequent rounds follow that mature account).
  - Sticky target is **still in its warm-up period, and no mature account is available in
    the pool**: as long as the warming-up account itself can still serve (has not hit its
    daily budget / concurrency / token-bucket ceiling), stickiness is kept to avoid
    pointlessly switching accounts within an all-warm-up pool and losing prompt cache for
    no benefit; a pool-wide reselection only happens once the account itself becomes
    unable to serve.
- **Fallback behavior**: providers this scheduler does not recognize (anything other
  than claude/codex), or any tier explicitly configured with weight `0`, are excluded
  from weighted candidacy and fall back to `Fallback` (default `RoundRobinSelector`) —
  identical to the behavior when the strategy is not enabled, with no impact on the
  existing request path for those providers.

Enabling this only requires changing `routing.strategy: "adaptive"`; the
`account-scheduling` section (`warmup-curve` / `mature-limits` / `tier-weights`) all has
built-in defaults (derived from an observed warm-up trajectory of a real production
account, see the comments in `config.example.yaml`), which can be overridden as needed;
field definitions are not repeated in this document.

## 2. Management API Field Reference

Every account object returned by `GET /v0/management/auth-files` now includes a new
read-only, purely-additive `adaptive_scheduling` sub-object (write site:
`internal/api/handlers/management/auth_files.go` line 490; projection logic:
`buildAdaptiveSchedulingView` in
`internal/api/handlers/management/auth_files_adaptive_scheduling.go`).

This is a **read-only projection**: it only reads data already persisted on the account
record (`quota_snapshot` / `first_production_at` under `Metadata`, `plan_type` under
`Attributes`) plus the warm-up curve config loaded at startup; it does not mint the
`first_production_at` anchor, does not write anything back to the auth record, and does
not trigger any upstream request.

Unknown state is always explicitly expressed as JSON `null` or an `"unknown"` label, and
is never silently guessed into a concrete value: a missing subscription tier, an
unreadable quota snapshot, or an account that hasn't been anchored yet will never be
disguised as `"pro"` / "0% used" / "just born" — callers such as the management frontend
and the farm orchestrator can rely on this to distinguish "not yet known" from "a real
value".

### 2.1 `adaptive_scheduling.subscription_tier` (string)

The account's fine-grained subscription tier:

- Claude value domain: `max_20x` / `max_5x` / `pro` / `unknown`.
- Codex value domain: `pro` / `plus` / `unknown`.
- When the provider is neither `claude` nor `codex`, this always returns the Claude-side
  `unknown` label (the two enums are fully independent and never share a value domain).

Source: Claude reads `Metadata.quota_snapshot.profile.organization.rate_limit_tier`;
Codex reads `Attributes.plan_type`. Both can be manually overridden by the top-level
`metadata.tier_override`, see section 3.1.

### 2.2 `adaptive_scheduling.quota_utilization` (object | null)

Structured per-quota-window utilization. When an account has no usable
`quota_snapshot.usage` snapshot at all (never probed / probe failed / this provider does
not poll quota), the whole field is `null` — it must never be read as "0% used".

- `windows` (object): keys are the upstream's original window names (e.g. `five_hour` /
  `seven_day` / `seven_day_sonnet`), values are:
  - `utilization_percent` (number, 0-100): the upstream's utilization percentage as-is,
    clamped to `[0,100]`.
  - `headroom` (number, 0-1): `1 - utilization_percent/100`, clamped to `[0,1]`.
  - `resets_at` (string, RFC3339 UTC, optional): the reset time for that window; the
    field is absent when the upstream does not provide a parseable timestamp.
- `binding_window` (object, optional): the single window, out of all windows, with the
  least headroom (the tightest one) — i.e. the window that actually constrains this
  account:
  - `window` (string): the bound window's name.
  - `headroom` (number, 0-1).
  - `resets_at` (string, RFC3339 UTC, optional).

Codex's `quota_snapshot.usage` shape has not yet been confirmed in this repository
against a real production account capture; known community reverse-engineering
information suggests it may nest windows under `rate_limit.primary_window` /
`secondary_window` and express them with `percent_left` rather than a top-level
`utilization` field — in that case the parser will most likely fail to recognize any
window, and `quota_utilization` will correctly show as `null` ("unknown") rather than
misreading `percent_left` as `utilization`.

### 2.3 `adaptive_scheduling.first_production_at` (string, RFC3339 UTC | null)

The account's freshness anchor. `null` when the account has never actually been put into
real service (not yet anchored). See section 3.2 for details.

### 2.4 `adaptive_scheduling.warmup` (object)

The warm-up/rate-limit stage the account is currently in:

- `stage` (string): the stage name. Its value is one of the `name`s configured in
  `account-scheduling.warmup-curve` (the default curve is `"w1"` / `"w2"` / `"w3-4"` /
  `"w5-6"` / `"w7-8"`), or one of two synthesized states: `"cold"` (the account has no
  `first_production_at` anchor yet, never actually put into production) /
  `"mature"` (the account's age has passed the entire `warmup-curve`, so
  `mature-limits` applies).
- `mature` (bool): whether the account has passed the entire warm-up curve and
  `mature-limits` applies. Both `"cold"` and any in-curve stage report `false`.
- `freshness_factor` (number, 0-1): the **observational view** of this stage's freshness
  factor — `cold` is fixed at `0`, rises linearly with account age inside the curve
  (strictly less than `1`), and `mature` is fixed at `1`. **Note**: this is only an
  independent view for operational observation, and is not the same implementation as
  the freshness factor that actually participates in the selection-weighting
  calculation — the selection side (`AccountFreshnessWeightFactor`) returns `1` for an
  account that is "not yet anchored" (a bootstrapping consideration: a new account that
  hasn't had a chance to go into production yet must not be permanently stuck riding
  along with the lowest weight, unable to even win the selection needed to anchor
  itself), while this observational field deliberately returns the most conservative `0`
  for that same case (an anti-ban-risk fallback signal — the `"cold"` label itself is
  meant to be a warning for humans). The two only deliberately diverge on this one case
  of "not yet anchored"; their numeric semantics agree everywhere else.
- `daily_budget` (number): the maximum number of requests allowed per UTC calendar day
  at this stage. `0` means no daily budget is set (`mature` is always `0`: driven purely
  by quota headroom, with no fixed daily budget; a curve stage itself can also be
  configured as `0` to mean unlimited at that stage).
- `rpm_limit` (number): the requests-per-minute rate-limit ceiling for this stage (i.e.
  the refill rate of the per-account token bucket).
- `concurrency_limit` (number): the maximum number of concurrent in-flight requests
  allowed at this stage.
- `age_days` (number | null): the account's whole-day age since `first_production_at`.
  `null` while in the `"cold"` (unanchored) state.

## 3. Operational Notes

### 3.1 The `tier_override` manual marker

Some real production accounts have an upstream `rate_limit_tier` value that the
auto-detection logic deliberately does not map (e.g. `default_claude_ai` — an
unrecognized value is never "guessed" into a known tier). Such accounts would otherwise
stay permanently at `subscription_tier = unknown` and be unable to participate in
tier-weighted selection. Operators can manually "pin" a tier:

- Edit that account's auth JSON file and add a string field `"tier_override"` to the
  top-level `"metadata"` object:
  - Legal Claude-side values: `"max_20x"` / `"max_5x"` / `"pro"`.
  - Legal Codex-side values: `"codex_pro"` / `"codex_plus"` (the `codex_` prefix
    distinguishes these from Claude's `"pro"`, avoiding the same key having conflicting
    meaning across the two providers).
  - The value is case-insensitive and automatically trimmed of leading/trailing
    whitespace; an empty value, an illegal value, or a value from the wrong provider
    (e.g. writing Claude's `"max_20x"` onto a Codex account) is ignored and automatically
    falls back to the original auto-detection path — existing behavior is completely
    unaffected when no legal override is present.
- This key is a **top-level** `metadata` field, not nested inside `quota_snapshot` — this
  is a deliberate design choice: quota polling refresh replaces the entire
  `quota_snapshot` sub-object wholesale, so a value written inside `quota_snapshot` would
  get overwritten and lost on the next refresh cycle (roughly every 45 minutes), whereas
  a value written at the top level is unaffected.
- There is currently no dedicated management write endpoint for setting this field; it
  can only be set by directly editing the account's auth JSON file.
- Typical use: auto-detection is inaccurate (as in the `default_claude_ai` case above),
  or when a specific tier's weighted-selection behavior needs to be manually
  simulated/tested.

### 3.2 The `first_production_at` anchor

This is the sole anchor for an account's freshness (warm-up age), determining which
stage of `warmup-curve` it falls into and its freshness weighting factor.

- **What it is**: the wall-clock instant this account was first successfully used to
  serve a real request. It is stamped once and never rewritten afterward.
- **When it is minted**: a downstream caller (the selection/execution path) calls
  `EnsureAuthFirstProductionAt` to mint and persist it on the account's first
  successful serving of a real request — failed requests, `count_tokens` preflight
  requests, and ephemeral Home dispatch deliberately never trigger minting; only an
  actually successful real serving counts. The management API projection described in
  sections 2.3/2.4 of this document is itself read-only and never proactively mints this
  anchor.
- **Append-only; never overwritten by quota refresh**: this field is deliberately
  **not** the auth file's mtime or `CreatedAt` — both of those get silently touched by
  unrelated token/quota refresh writes, or by a re-auth (which replaces the underlying
  file), and neither stays stable across an account's entire lifetime.
  `first_production_at` lives in the same `Metadata` map (alongside `quota_snapshot` /
  `rate_limit_tier`), but is only (re-)written the one time it has never been set before,
  or when the stored value is corrupt/unparseable; any subsequent call never overwrites
  an existing legal value.
- **Effect on warm-up tier**: account age = current time - this anchor, truncated to a
  whole number of days and looked up against each `warmup-curve` stage's
  `[min-age-days, max-age-days)` range; once past the last stage, the account enters
  `mature-limits`. An account **without** this anchor (the `"cold"` state) is confined to
  the rate-limit thresholds of the curve's **first stage** (i.e. the strictest one), but
  its freshness factor on the selection-weighting side is treated as `1` (see the
  divergence note in section 2.4), so that such accounts still get a chance to win a
  selection and thereby complete their own anchoring.

## 4. Caveats

- **New accounts are protected during warm-up**: new accounts (`"cold"` or an early stage
  within the curve) have their daily budget/RPM/concurrency pushed very low (the
  default curve's first stage is only 200/day, 3 RPM, concurrency 1), deliberately far
  below mature accounts, so that burst traffic naturally routes to mature accounts
  instead of concentrating on the newest, most fragile accounts.
- **Mature accounts have relaxed limits**: once an account passes the entire
  `warmup-curve` (age >= 60 days by default) it enters `mature-limits`: no more fixed
  daily budget, driven instead by quota headroom; RPM/concurrency/burst ceilings are also
  relaxed to a level that deliberately leaves headroom and only intercepts pathological
  bursts (not a level normal throughput would ever hit).
- **Rate-limit/daily-budget counters are rebuilt from in-memory, non-persistent state
  after a restart**: the per-account token bucket (`AccountRateLimiter`), the daily
  request counter, and the in-flight concurrency counter (`AccountConcurrencyGate`) all
  live purely in process memory — **none of it is written to disk or persisted**. After
  a process restart:
  - the token bucket starts fresh from a "full bucket" state (allowing the configured
    burst to be used up all at once);
  - the day's request counter and in-flight concurrency counter both reset to zero and
    start accumulating again.
  This means the practical effect across a restart boundary is **biased toward being
  more permissive, not more conservative** — for example, if an account has already hit
  its `daily_budget` for the day, a process restart zeroes that counter, and the account
  effectively regains a fresh round of budget headroom for the rest of that day. This is
  a deliberately accepted "safe direction to err in" by design (better to occasionally
  allow a bit more across a restart boundary than to build a separate persistence
  subsystem for what is fundamentally short-lived counter state) — it is not a defect.
  - In contrast, the **`first_production_at` anchor is persisted** (written into the
    `metadata` of the account's auth JSON file) and is completely unaffected by a process
    restart — the age judgment that determines warm-up tier stays consistent across
    restarts; only the "counter" state for rate limiting/daily budget/concurrency gets
    rebuilt.
