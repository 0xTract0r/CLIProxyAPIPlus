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

### 2.5 `account_scheduling` namespace rename and the additional projection fields

Since the §8.5 namespace unification the projection is surfaced under the object
name **`account_scheduling`** (the canonical name the cpamp frontend reads); the
legacy name **`adaptive_scheduling`** is still emitted alongside it with the
byte-identical value as a transition-safety measure (dual-emit site:
`internal/api/handlers/management/auth_files.go`). Prefer reading
`account_scheduling`; both objects carry every field in sections 2.1–2.4 plus the
fields below (projection: `buildAccountSchedulingView`).

- `tier_source` (string, `"auto"` | `"override"`): whether `subscription_tier`
  (section 2.1) came from a manual, provider-appropriate `tier_override`
  (`"override"`) or from `rate_limit_tier` / `chatgpt_plan_type` auto-detection
  (`"auto"`). Derived on read from the override's presence — it is not a separately
  persisted field. A blank, malformed, or cross-provider override reads `"auto"`,
  matching the fact that the tier resolvers ignore exactly those. This is the field
  a frontend reads to show a "manual" tier badge.
- `rate_scale` (number, > 0): the effective per-account rate multiplier applied to
  this account's derived rate ceilings (see section 3.4). `1.0` means no scaling.
- `in_distress` (bool): whether the account currently shows an early risk-control
  signal per the health gate (see section 3.6). Purely observational; `false` when
  the gate is disabled.
- `warmup_health_stage_cap` (number | null): the persisted health-allowed maximum
  warm-up stage index (`0` = the first / strictest stage). `null` when no cap is
  recorded (the account has never been decelerated) — never read `null` as "capped
  to stage 0".
- `warmup_last_distress_at` (string, RFC3339 UTC | null): wall-clock of the most
  recent recorded distress. `null` when the account has never shown distress.
- `sessions_total` / `sessions_active` / `sessions_closed` (number): distinct
  session-id counts observed on this account's recorded requests, bucketed by idle
  time (active within 10 min, closed after 30 min, by default). `0` can mean either
  "no sessions observed yet" or "no usage store wired" — the two are deliberately
  not distinguished (zero sessions is itself a valid answer, unlike the
  missing-snapshot ambiguity of `quota_utilization`).

Interaction with section 2.4: once the health gate is active, `warmup.stage` /
`rpm_limit` / `daily_budget` / `concurrency_limit` reflect the **effective**
(health-capped) stage, while `warmup.age_days` still reports the raw calendar age —
so a reader can render "aged N days but decelerated to `<stage>`" (see section 3.6).

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
- A dedicated admin management endpoint now sets/clears this field at runtime:
  `PATCH /v0/management/auth-files/account-scheduling` (see section 3.5). Hand-editing
  the auth JSON still works (the legacy bare top-level key is dual-read), but the
  canonical write location is now the namespaced `account_scheduling` object (see
  section 3.3); the endpoint writes there and reloads the running selector.
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
- **Operator backfill channel**: beyond the append-only auto-mint above, an operator
  can explicitly set or clear this anchor via
  `PATCH /v0/management/auth-files/account-scheduling` (see section 3.5). This exists
  to migrate accounts that were already aged/in-production **before** adaptive
  scheduling was enabled — the auto-mint would otherwise stamp them as brand-new and
  clamp them to the strictest warm-up stage. Only a future timestamp is rejected; any
  past date is accepted and the operator owns its correctness. Setting an anchor
  **earlier** than the truth makes the account look more mature than it is (less
  warm-up), which is the account-safety-risky direction, so only backfill dates you
  have actually confirmed. Clearing it re-opens the auto-mint (the next real serving
  success re-stamps a fresh anchor).

### 3.3 The `account_scheduling` metadata namespace (dual-read / dual-emit)

All operator/auto scheduling state lives under a single **top-level**
`account_scheduling` object in the account's auth JSON `metadata`. Persisted
sub-keys: `tier_override`, `first_production_at`, `rate_scale`,
`warmup_health_stage_cap`, `warmup_last_distress_at` (`tier_source` is derived on
read for the projection, not stored). Keeping them under one top-level object — not
nested inside `quota_snapshot` — is what makes them survive the ~45-minute quota
refresh: that refresh replaces the whole `quota_snapshot` sub-object, but
`Auth.Clone` copies every top-level metadata key through untouched.

- **Dual-read (migration)**: reads prefer the namespaced sub-key and fall back to a
  legacy **bare top-level** key. Only `tier_override` and `first_production_at` had a
  pre-§8.5 bare form, so only those two are dual-read; `rate_scale`,
  `warmup_health_stage_cap` and `warmup_last_distress_at` were introduced inside the
  object and have no bare form.
- **Write goes only to the new location**: minting/setting always writes the
  namespaced sub-key, never the bare key — a non-destructive migration (an existing
  legacy value is honored on read but never rewritten).
- **Clear deletes both locations**: clearing any of these removes both the namespaced
  sub-key and the legacy bare key, so a stale bare value cannot resurface through
  dual-read on the next refresh.
- **Projection dual-emit**: the account-list response emits both `account_scheduling`
  (canonical) and `adaptive_scheduling` (legacy, identical value) — see section 2.5.

### 3.4 `rate_scale`: per-account safety-test rate multiplier

`rate_scale` is a speed multiplier applied to an account's **derived** rate ceilings
— rpm / burst / concurrency / daily budget — **after** the tier/warm-up derivation.
It is deliberately **independent of selection weight**: it never changes *which*
account the selector picks, only how fast the picked account may go.

- **Config default**: `account-scheduling.rate-scale` (float, default **`1.0`**).
- **Per-account override**: metadata `account_scheduling.rate_scale` (dual-read also
  honors a legacy bare `rate_scale`), settable via the endpoint in section 3.5.
- **Resolution order**: a valid per-account override (present and `> 0`) → the config
  default (when `> 0`) → `1.0`. A non-positive or unparseable value at any layer is
  skipped in favor of the next, so the effective multiplier is always `> 0` and `1.0`
  is always a safe no-op.
- **`1.0` = no effect**; `< 1` throttles every ceiling below its tier/warm-up value
  (for low-risk safety testing); `> 1` lifts it.
- **Applies during warm-up too**: it scales whichever ceiling the account currently
  sits at (a warming stage or the mature ceiling), so it is not limited to mature
  accounts.
- **Floors keep a limit positive, never zero**: config load rejects a non-positive
  `rate-scale`; on the read path a fractional scale rounds a derived integer ceiling
  to the nearest whole unit and floors it at `1`, so a small scale can throttle an
  account but can never wedge a positive limit to a permanent `0`. A non-positive /
  unbounded ceiling (e.g. a mature account's `0` = unlimited daily budget) is left
  unchanged.

### 3.5 Management endpoint: `PATCH /v0/management/auth-files/account-scheduling`

Admin-gated (the same `/v0/management` admin auth as its sibling auth-file
endpoints — `X-Management-Key: <key>` or `Authorization: Bearer <key>`, no new
exemption). It sets or clears an account's operator overrides — `tier_override`,
`rate_scale`, `first_production_at` — at runtime, persists them, makes the running
selector observe them (`authManager.Update`), and returns the refreshed projection.

Request body (`application/json`):

- `name` (string, **required**): auth id / filename / display name.
- `auth_index` (string, optional): disambiguation.
- `tier_override` / `rate_scale` / `first_production_at`: field **presence** drives
  intent — absent = leave untouched; explicit empty string or JSON `null` = clear; a
  value = set. **At least one** of the three must be present, else `400`.

Per-provider validation:

- `tier_override`: legal for the account's provider (claude: `max_20x` / `max_5x` /
  `pro`; codex: `codex_pro` / `codex_plus`), else `400` with a `legal_values` list.
- `rate_scale`: a number `> 0`, else `400`.
- `first_production_at`: RFC3339 **and not in the future**, else `400`; any past date
  is accepted.

Clearing double-deletes (namespaced + legacy bare, to prevent a stale value
resurfacing): clearing `tier_override` lets `tier_source` fall back to `"auto"`;
clearing `rate_scale` falls back to the config default (else `1.0`); clearing
`first_production_at` re-opens the append-only auto-mint. Other responses: `400`
invalid body / no override field; `404` account not found; `409` plugin-virtual
auth; `503` auth manager unavailable; `500` persist failure.

Set example (pin tier, throttle to half rate, backfill anchor):

```bash
curl -sS -X PATCH https://<host>/v0/management/auth-files/account-scheduling \
  -H "X-Management-Key: <management-key>" \
  -H "Content-Type: application/json" \
  -d '{
        "name": "AC-14.json",
        "tier_override": "max_5x",
        "rate_scale": 0.5,
        "first_production_at": "2026-06-01T00:00:00Z"
      }'
```

Clear example (empty string or `null` clears; leaves `first_production_at` untouched):

```bash
curl -sS -X PATCH https://<host>/v0/management/auth-files/account-scheduling \
  -H "X-Management-Key: <management-key>" \
  -H "Content-Type: application/json" \
  -d '{ "name": "AC-14.json", "tier_override": "", "rate_scale": null }'
```

Success `200` response (the `account_scheduling` value is the full refreshed
projection — sections 2.1–2.5):

```json
{
  "name": "AC-14",
  "account_scheduling": {
    "subscription_tier": "max_5x",
    "tier_source": "override",
    "rate_scale": 0.5,
    "quota_utilization": { "windows": { }, "binding_window": { } },
    "first_production_at": "2026-06-01T00:00:00Z",
    "warmup": {
      "stage": "w7-8", "mature": false, "freshness_factor": 0.9,
      "daily_budget": 6500, "rpm_limit": 30, "concurrency_limit": 3, "age_days": 55
    },
    "in_distress": false,
    "warmup_health_stage_cap": null,
    "warmup_last_distress_at": null,
    "sessions_total": 0, "sessions_active": 0, "sessions_closed": 0
  }
}
```

### 3.6 Health-gated warm-up ramp (ANCHOR-Q4)

Warm-up promotion is no longer purely age-based: an account climbs the warm-up curve
by **age *and* health**. The effective warm-up stage is
`min(age-based stage, health-allowed stage cap)`; an early risk-control signal clamps
the cap down and holds it there until the account recovers. Claude accounts only
(Codex/xAI/Gemini are not under adaptive warm-up, so the write path never records a
cap for them).

Key invariants:

- **Only ever lowers, never raises** (fail-safe): the gate can never push an account
  above the stage its age already earns.
- **Distress decelerates**: on a distress hit the cap drops by `demote-step` stages
  (floored at the strictest stage) and `warmup_last_distress_at` is stamped. Both the
  grading view (rpm / daily budget / concurrency) and the selection-weight view
  (freshness factor / maturity) see the lowered stage from one shared read-side clamp.
- **Recovery is slow and health-gated**: only a healthy *success*, with a cap present
  and at least `promote-cooldown-minutes` since the last distress, raises the cap by
  **one** stage; each step re-arms the cooldown, so at most one stage is regained per
  cooldown window. Once the cap reaches the age-deserved stage it is dropped entirely
  (pure age again). Promotion requires health + cooldown — an account cannot climb by
  simply aging while it keeps failing.
- **Mature accounts are out of scope** (never demoted): an account past the whole
  curve is governed by pure age; a mature account showing distress is already covered
  by cooldown / quota-deweight / auto-quarantine and is deliberately not pushed back
  into a warm-up daily-budget hard gate.
- **Cold accounts are skipped**: an un-anchored ("cold") account carries no cap.

Config (`account-scheduling.health-gate.*`; defaults are conservative fail-safe
values — design pins no exact numbers, pending calibration against real 201 data):

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Master switch. `false` = effective stage always equals the age stage (pre-ANCHOR-Q4 behavior). |
| `failure-cluster-threshold` | `3` | Failed requests within the observation window that mark distress. `0` disables this signal. |
| `backoff-level-threshold` | `1` | `Quota.BackoffLevel` (escalating plan-quota 429 backoff exponent) at or above which distress is marked. `1` = any active plan-quota backoff. `0` disables this signal. |
| `observation-window-minutes` | `30` | How far back the failure cluster is summed (only meaningful when `failure-cluster-threshold > 0`). |
| `demote-step` | `1` | Warm-up stages the cap drops per distress hit (must be `>= 1` when enabled). |
| `promote-cooldown-minutes` | `30` | Minimum time since the last distress before a healthy success may re-raise the cap by one stage (must be `> 0` when enabled). |

The two distress signals are **OR-ed** — either the failure cluster or the backoff
level alone marks distress. When `enabled`, config load requires at least one of the
two thresholds to be positive.

Operational reads (projection, section 2.5): `in_distress` (currently signalling),
`warmup_health_stage_cap` (the persisted cap, `null` = none), and
`warmup_last_distress_at` (when it last decelerated). Compare `warmup.stage`
(effective, capped) against `warmup.age_days` (raw age) to spot an account that has
been decelerated below its age.

Signal fidelity (v1): the gate uses only the two signals core already tracks —
recent-request failure clusters and `Quota.BackoffLevel` — and adds **no** precise
429 classification. Hard failures (quarantine / reauth / active cooldown) are not
this layer's concern; they are already filtered out of the selectable pool. The
thresholds above are conservative defaults awaiting calibration against real 201
traffic.

### 3.7 Reading the account page (cpamp management UI)

The cpamp account page renders the section 2.5 projection fields directly; operators
read them as:

- **Subscription-tier badge** (`20x` / `5x` / `Pro` / `unknown`):
  `subscription_tier`. `unknown` is expected, not a bug, when the upstream returns an
  unmapped `rate_limit_tier` (e.g. `default_claude_ai`) — pin a tier via
  `tier_override` (sections 3.1 / 3.5) if the account should participate in
  tier-weighted selection.
- **Warm-up badge**: `warmup.stage` (effective/capped stage) with `warmup.age_days`.
- **"Manual" marker**: `tier_source = "override"`.
- **Session counts**: `sessions_total` / `sessions_active` / `sessions_closed`.
- **Decelerated state**: `in_distress = true` (with `warmup_health_stage_cap` /
  `warmup_last_distress_at`) means the health gate has slowed this account down.

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
