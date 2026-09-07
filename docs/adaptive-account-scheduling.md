# Adaptive Account Scheduling

> Implements `openspec/changes/add-adaptive-account-scheduling`. Config field definitions
> are already fully covered in the `account-scheduling` section of
> `core/config.example.yaml` and are not repeated here; this document only adds the
> management API read-only field reference and operational notes. Fields/behavior have
> been verified against the current code as of 2026-09. Code/symbol locations are
> consolidated in the [Code index](#code-index) at the end, not inlined after each claim.

## Operator quick reference

Common operator tasks and where each one lives:

| I want to… | Use | Where |
| --- | --- | --- |
| Turn adaptive scheduling on | `routing.strategy: "adaptive"` | §1 |
| Read an account's tier / quota / warm-up state | `GET /v0/management/auth-files` → the `account_scheduling` projection | §2 |
| Pin a subscription tier on one account | `tier_override` (endpoint or auth JSON) | §3.1 / §3.5 |
| Migrate an old account / backfill its warm-up anchor | `first_production_at` (endpoint backfill) | §3.2 / §3.5 |
| Slow-speed safety-test one account | `rate_scale` (`< 1` throttles) | §3.4 / §3.5 |
| See why one account was decelerated | `in_distress` / `warmup_health_stage_cap` / `warmup_last_distress_at`; compare `warmup.stage` vs `warmup.age_days` | §3.6 / §3.7 |
| Change an operator override (tier / rate / anchor) | `PATCH /v0/management/auth-files/account-scheduling` | §3.5 |

## 1. Overview

> **Scope: Claude accounts only.** This feature — the scheduling projection, weighted
> selection, adaptive warm-up, and the management endpoint below — applies only to Claude
> accounts. Non-Claude accounts (Codex, Gemini, xAI, etc.) are completely unaffected:
> `GET /v0/management/auth-files` never returns an `account_scheduling` projection object
> for them.

Adaptive scheduling picks *which* account serves each request by scoring accounts on
subscription capacity, live quota, and freshness — instead of plain round-robin.

**How to enable.** Set `routing.strategy: "adaptive"` (the default stays `round-robin`).
When it is not set, the whole `account-scheduling` section has no effect and is ignored.
That section (`warmup-curve` / `mature-limits` / `tier-weights`) all ships with built-in
defaults (derived from a real production account's observed warm-up trajectory; see the
comments in `config.example.yaml`) and can be overridden as needed. Field definitions live
in `config.example.yaml`, not here.

Once enabled, a single account selection is jointly determined by the layers below.

### Weighted selection: tier capacity x live quota headroom x freshness

Core formula:

```text
weight = tier base capacity weight x (1 - quota utilization) x freshness factor
```

- **Tier base capacity weight** is configured per Claude subscription tier:
  - Claude has four tiers: `max_20x` / `max_5x` / `pro` / `unknown`.
- **Quota headroom** is taken from the account's tightest quota window (the highest
  utilization), not from an arbitrary window or an average.
  - Example: an account whose `five_hour` window is already at 90% is not treated as
    safely available just because its `seven_day` window still has headroom.
  - When the quota snapshot is entirely unreadable: a neutral, conservatively-biased
    fallback value is used — it is never treated as "0% used".
- **Freshness factor** is derived from the account's age since `first_production_at`
  (§3.2), looked up against the stages of `account-scheduling.warmup-curve`.
  - In-curve accounts get a factor `< 1`.
  - Accounts that have passed the whole curve get a factor of `1`.

### Per-account rate-limit smoothing (not a global pool)

Each account has its own independent token bucket, so one client hammering a single
account cannot squeeze the quota of the other accounts in the pool.

- Rate/burst capacity comes from whichever warm-up stage (or mature state) the account
  is currently in — its `rpm-limit` / concurrency ceiling.
- A momentarily rate-limited candidate is not rejected outright; weighted sampling
  simply continues over the remaining candidate pool.
- "Burst traffic naturally routes to mature accounts" falls out of weight + token bucket
  acting together — no separate burst-detection logic is required.

### Progressive ramp-up during a new account's warm-up period

New accounts have `daily-budget` / `rpm-limit` / `concurrency-limit` raised week by week.

- The ramp follows the stages configured in `warmup-curve` (5 stages by default; see
  `config.example.yaml`).
- Past the last stage (age >= 60 days by default) the account enters `mature-limits`: no
  fixed daily budget, driven by quota headroom instead.

### Tiered session stickiness

When `routing.session-affinity: true`, this selector carries its own session-stickiness
cache and tiers by account maturity — unlike the outer generic session-affinity wrapper,
which simply returns immediately on a cache hit and never consults the inner selector.

| Sticky target state | Behavior |
| --- | --- |
| **Non-Claude account** (a provider this scheduler does not score) | No tiering; behavior is identical to existing session-affinity. |
| **Mature, soft ceiling not hit** (token bucket still permits it) | Stickiness kept, preserving prompt-cache continuity. |
| **Mature, soft ceiling already hit** (treated as approaching the hard risk-control threshold) | Stickiness broken; a fresh weighted selection is made across the whole pool. |
| **Still warming up, a usable mature account exists** | Stickiness broken; routed to a mature account, with rebinding (subsequent rounds follow that mature account). |
| **Still warming up, no mature account available** | As long as the warming account can still serve (daily budget / concurrency / token bucket not maxed), stickiness is kept — avoids pointless churn and prompt-cache loss in an all-warm-up pool. A pool-wide reselection happens only once the account itself can no longer serve. |

### Fallback behavior

Providers this scheduler does not recognize (anything other than claude), or any
tier explicitly configured with weight `0`, are excluded from weighted candidacy and fall
back to `Fallback` (default `RoundRobinSelector`) — identical to the behavior when the
strategy is not enabled, with no impact on the existing request path for those providers.

## 2. Management API field reference

`GET /v0/management/auth-files` now includes a read-only, purely-additive scheduling
projection on every account object.

This is a **read-only projection**:

- It reads only data already persisted on the account record — `Metadata.quota_snapshot`
  / `Metadata.first_production_at` and `Attributes.plan_type` — plus the warm-up curve
  config loaded at startup.
- It does not mint the `first_production_at` anchor, does not write anything back to the
  auth record, and does not trigger any upstream request.

Unknown state is always explicit and never silently guessed:

- It is expressed as JSON `null` or an `"unknown"` label.
- A missing subscription tier, an unreadable quota snapshot, or an un-anchored account is
  never disguised as `"pro"` / "0% used" / "just born".
- Callers such as the management frontend and the farm orchestrator can rely on this to
  distinguish "not yet known" from a real value.

### 2.1 `subscription_tier` (string)

The account's fine-grained subscription tier.

- This field exists for Claude accounts only.
- Value domain: `max_20x` / `max_5x` / `pro` / `unknown`.
- Source: `Metadata.quota_snapshot.profile.organization.rate_limit_tier`. Can be manually
  overridden by the top-level `metadata.tier_override` (§3.1).

### 2.2 `quota_utilization` (object | null)

Structured per-quota-window utilization.

The whole field is `null` when the account has no usable `quota_snapshot.usage` snapshot
(never probed / probe failed / this provider does not poll quota) — it must never be read
as "0% used".

| Field | Type | Meaning |
| --- | --- | --- |
| `windows` | object | Keys are the upstream's original window names (e.g. `five_hour` / `seven_day` / `seven_day_sonnet`). |
| `windows.<w>.utilization_percent` | number, 0-100 | The upstream's utilization percentage as-is, clamped to `[0,100]`. |
| `windows.<w>.headroom` | number, 0-1 | `1 - utilization_percent/100`, clamped to `[0,1]`. |
| `windows.<w>.resets_at` | string, RFC3339 UTC, optional | Reset time for that window; absent when the upstream gives no parseable timestamp. |
| `binding_window` | object, optional | The single window with the least headroom (the tightest) — the one that actually constrains this account. |
| `binding_window.window` | string | The bound window's name. |
| `binding_window.headroom` | number, 0-1 | |
| `binding_window.resets_at` | string, RFC3339 UTC, optional | |

### 2.3 `first_production_at` (string, RFC3339 UTC | null)

The account's freshness anchor.

- `null` when the account has never actually been put into real service (not yet
  anchored).
- See §3.2 for details.

### 2.4 `warmup` (object)

The warm-up / rate-limit stage the account is currently in.

| Field | Type | Meaning |
| --- | --- | --- |
| `stage` | string | Current stage name: one of the `warmup-curve` stage `name`s (default curve `"w1"` / `"w2"` / `"w3-4"` / `"w5-6"` / `"w7-8"`), or a synthesized state — `"cold"` (no `first_production_at` anchor yet, never produced) / `"mature"` (age past the whole curve, so `mature-limits` applies). |
| `mature` | bool | Whether the account has passed the whole curve and `mature-limits` applies. Both `"cold"` and any in-curve stage report `false`. |
| `freshness_factor` | number, 0-1 | The **observational** view of this stage's freshness factor (see the divergence note below). `cold` = `0`; rises linearly with age in-curve (strictly `< 1`); `mature` = `1`. |
| `daily_budget` | number | Max requests per UTC calendar day at this stage. `0` = no daily budget (`mature` is always `0`: driven purely by quota headroom; an in-curve stage can also be configured `0` = unlimited at that stage). |
| `rpm_limit` | number | Requests-per-minute rate-limit ceiling (the refill rate of the per-account token bucket). |
| `concurrency_limit` | number | Max concurrent in-flight requests allowed at this stage. |
| `age_days` | number \| null | Whole-day age since `first_production_at`. `null` while in the `"cold"` (unanchored) state. |

**Divergence note on `freshness_factor`.** This field is an independent view for
operational observation; it is not the same implementation as the freshness factor that
actually participates in selection weighting. The two deliberately diverge on exactly one
case — an account that is "not yet anchored":

- Selection side (`AccountFreshnessWeightFactor`): returns `1`. A bootstrapping
  consideration — a new account that hasn't had a chance to go into production yet must
  not be permanently stuck riding along with the lowest weight, unable to even win the
  selection needed to anchor itself.
- This observational field: returns the most conservative `0`. An anti-ban-risk fallback
  signal — the `"cold"` label itself is meant to be a warning for humans.

Everywhere else, their numeric semantics agree.

### 2.5 `account_scheduling` namespace rename and the additional projection fields

Since the §8.5 namespace unification, the projection is surfaced under the object name
**`account_scheduling`** (the canonical name the cpamp frontend reads). The legacy name
**`adaptive_scheduling`** is still emitted alongside it with the byte-identical value as a
transition-safety measure.

Prefer reading `account_scheduling`. Both objects carry every field in §2.1–2.4 plus the
fields below (projection builder: `buildAccountSchedulingView`).

| Field | Type | Meaning |
| --- | --- | --- |
| `tier_source` | `"auto"` \| `"override"` | Whether `subscription_tier` (§2.1) came from a manual, provider-appropriate `tier_override` (`"override"`) or from `rate_limit_tier` / `chatgpt_plan_type` auto-detection (`"auto"`). Derived on read from the override's presence — not a separately persisted field. A blank, malformed, or cross-provider override reads `"auto"`, matching the fact that the tier resolvers ignore exactly those. This is the field a frontend reads to show a "manual" tier badge. |
| `rate_scale` | number, > 0 | The effective per-account rate multiplier applied to this account's derived rate ceilings (§3.4). `1.0` = no scaling. |
| `in_distress` | bool | Whether the account currently shows an early risk-control signal per the health gate (§3.6). Purely observational; `false` when the gate is disabled. |
| `warmup_health_stage_cap` | number \| null | The persisted health-allowed maximum warm-up stage index (`0` = the first / strictest stage). `null` when no cap is recorded (the account has never been decelerated) — never read `null` as "capped to stage 0". |
| `warmup_last_distress_at` | string, RFC3339 UTC \| null | Wall-clock of the most recent recorded distress. `null` when the account has never shown distress. |
| `sessions_total` / `sessions_active` / `sessions_closed` | number | Distinct session-id counts observed on this account's recorded requests, bucketed by idle time (active within 10 min, closed after 30 min, by default). `0` can mean either "no sessions observed yet" or "no usage store wired" — the two are deliberately not distinguished (zero sessions is itself a valid answer, unlike the missing-snapshot ambiguity of `quota_utilization`). |

**Interaction with §2.4.** Once the health gate is active, `warmup.stage` / `rpm_limit` /
`daily_budget` / `concurrency_limit` reflect the **effective** (health-capped) stage,
while `warmup.age_days` still reports the raw calendar age — so a reader can render
"aged N days but decelerated to `<stage>`" (§3.6).

## 3. Operational notes

### 3.1 `tier_override`: pin a subscription tier manually

Manually pin a tier when auto-detection cannot map the upstream value.

Some real production accounts return a `rate_limit_tier` that the auto-detection logic
deliberately does not map (e.g. `default_claude_ai` — an unrecognized value is never
"guessed" into a known tier). Such accounts would otherwise stay permanently at
`subscription_tier = unknown` and be unable to participate in tier-weighted selection.

How to set it:

- Set it via the endpoint (§3.5), or hand-edit the auth JSON by adding a string field
  `"tier_override"` to the top-level `"metadata"` object.
- Legal values: `"max_20x"` / `"max_5x"` / `"pro"`.
- The value is case-insensitive and automatically trimmed. An empty value or an illegal
  value is ignored and falls back to the auto-detection path — existing behavior is
  completely unaffected when no legal override is present.

Why it lives at the top level, not inside `quota_snapshot`: quota polling refresh (roughly
every 45 minutes) replaces the entire `quota_snapshot` sub-object wholesale, so a value
written inside it would get overwritten and lost on the next refresh; a value written at
the top level is unaffected.

Canonical write location: the endpoint `PATCH /v0/management/auth-files/account-scheduling`
(§3.5) is the canonical setter — it writes to the namespaced `account_scheduling` object
(§3.3) and reloads the running selector. Hand-editing the auth JSON still works (the legacy
bare top-level key is dual-read).

Typical use: auto-detection is inaccurate (as in the `default_claude_ai` case above), or a
specific tier's weighted-selection behavior needs to be manually simulated/tested.

### 3.2 `first_production_at`: the freshness anchor

This is the sole anchor for an account's freshness (warm-up age). It decides which
`warmup-curve` stage the account falls into and its freshness weighting factor.

- **What it is**: the wall-clock instant this account first successfully served a real
  request. It is stamped once and never rewritten afterward.
- **When it is minted**: a downstream caller (the selection/execution path) calls
  `EnsureAuthFirstProductionAt` on the first successful serving of a real request. It is
  deliberately never minted by a failed request, a `count_tokens` preflight request, or
  ephemeral Home dispatch — only an actually successful real serving counts. The §2.3/§2.4
  projection is itself read-only and never mints this anchor.
- **Append-only; never overwritten by quota refresh**:
  - It is deliberately **not** the auth file's mtime or `CreatedAt` — both get silently
    touched by unrelated token/quota refresh writes or by a re-auth (which replaces the
    underlying file), so neither stays stable across an account's whole lifetime.
  - It lives in the same `Metadata` map (alongside `quota_snapshot` / `rate_limit_tier`),
    but is (re-)written only the one time it has never been set before, or when the stored
    value is corrupt/unparseable. Any subsequent call never overwrites an existing legal
    value.
- **Effect on warm-up stage**:
  - Account age = current time − this anchor, truncated to whole days and looked up
    against each `warmup-curve` stage's `[min-age-days, max-age-days)` range; past the last
    stage, the account enters `mature-limits`.
  - An account **without** this anchor (the `"cold"` state) is confined to the rate-limit
    thresholds of the curve's **first (strictest) stage**, but its freshness factor on the
    selection-weighting side is treated as `1` (see the §2.4 divergence note), so such
    accounts still get a chance to win a selection and complete their own anchoring.
- **Operator backfill channel**:
  - Beyond the append-only auto-mint above, an operator can explicitly set or clear this
    anchor via the endpoint (§3.5).
  - Purpose: migrate accounts already aged/in-production **before** adaptive scheduling was
    enabled — otherwise auto-mint would stamp them brand-new and clamp them to the
    strictest warm-up stage.
  - Validation: only a future timestamp is rejected; any past date is accepted and the
    operator owns its correctness.
  - Safety direction: setting an anchor **earlier** than the truth makes the account look
    more mature than it is (less warm-up), which is the account-safety-risky direction, so
    only backfill dates you have actually confirmed.
  - Clearing re-opens the auto-mint (the next real serving success re-stamps a fresh
    anchor).

### 3.3 `account_scheduling` metadata namespace (dual-read / dual-emit)

All operator/auto scheduling state lives under a single **top-level** `account_scheduling`
object in the account's auth JSON `metadata`.

- Persisted sub-keys: `tier_override`, `first_production_at`, `rate_scale`,
  `warmup_health_stage_cap`, `warmup_last_distress_at`. (`tier_source` is derived on read
  for the projection, not stored.)
- Why one top-level object, not nested inside `quota_snapshot`: that is what makes them
  survive the ~45-minute quota refresh. The refresh replaces the whole `quota_snapshot`
  sub-object, but `Auth.Clone` copies every top-level metadata key through untouched.

Behavior:

- **Dual-read (migration)**: reads prefer the namespaced sub-key and fall back to a legacy
  **bare top-level** key. Only `tier_override` and `first_production_at` had a pre-§8.5
  bare form, so only those two are dual-read; `rate_scale`, `warmup_health_stage_cap` and
  `warmup_last_distress_at` were introduced inside the object and have no bare form.
- **Write goes only to the new location**: minting/setting always writes the namespaced
  sub-key, never the bare key — a non-destructive migration (an existing legacy value is
  honored on read but never rewritten).
- **Clear deletes both locations**: clearing any of these removes both the namespaced
  sub-key and the legacy bare key, so a stale bare value cannot resurface through dual-read
  on the next refresh.
- **Projection dual-emit**: the account-list response emits both `account_scheduling`
  (canonical) and `adaptive_scheduling` (legacy, identical value) — see §2.5.

### 3.4 `rate_scale`: per-account safety-test rate multiplier

A per-account speed multiplier for low-risk safety testing. It changes how fast a picked
account may go, never *which* account gets picked.

`rate_scale` is applied to an account's **derived** rate ceilings — rpm / burst /
concurrency / daily budget — **after** the tier/warm-up derivation, and is deliberately
**independent of selection weight**.

- **Config default**: `account-scheduling.rate-scale` (float, default **`1.0`**).
- **Per-account override**: metadata `account_scheduling.rate_scale` (dual-read also honors
  a legacy bare `rate_scale`), settable via the §3.5 endpoint.
- **Resolution order**: a valid per-account override (present and `> 0`) → the config
  default (when `> 0`) → `1.0`. A non-positive or unparseable value at any layer is skipped
  in favor of the next, so the effective multiplier is always `> 0` and `1.0` is always a
  safe no-op.
- **Meaning**: `1.0` = no effect; `< 1` throttles every ceiling below its tier/warm-up
  value (for low-risk safety testing); `> 1` lifts it.
- **Applies during warm-up too**: it scales whichever ceiling the account currently sits at
  (a warming stage or the mature ceiling), so it is not limited to mature accounts.
- **Floors keep a limit positive, never zero**: config load rejects a non-positive
  `rate-scale`; on the read path a fractional scale rounds a derived integer ceiling to the
  nearest whole unit and floors it at `1`, so a small scale can throttle an account but can
  never wedge a positive limit to a permanent `0`. A non-positive / unbounded ceiling (e.g.
  a mature account's `0` = unlimited daily budget) is left unchanged.

### 3.5 Management endpoint: `PATCH /v0/management/auth-files/account-scheduling`

The runtime setter/clearer for an account's operator overrides — `tier_override`,
`rate_scale`, `first_production_at`. It persists them, makes the running selector observe
them (`authManager.Update`), and returns the refreshed projection.

Auth: admin-gated with the same `/v0/management` admin auth as its sibling auth-file
endpoints — `X-Management-Key: <key>` or `Authorization: Bearer <key>`, no new exemption.

Request body (`application/json`):

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | **yes** | auth id / filename / display name. |
| `auth_index` | no | disambiguation. |
| `tier_override` / `rate_scale` / `first_production_at` | **≥ 1 present** | Field **presence** drives intent — absent = leave untouched; explicit empty string or JSON `null` = clear; a value = set. At least one of the three must be present, else `400`. |

Validation:

- `tier_override`: must be one of `max_20x` / `max_5x` / `pro`, else `400` with a
  `legal_values` list.
- `rate_scale`: a number `> 0`, else `400`.
- `first_production_at`: RFC3339 **and not in the future**, else `400`; any past date is
  accepted.

Clearing double-deletes (namespaced + legacy bare, to prevent a stale value resurfacing):

- Clearing `tier_override` lets `tier_source` fall back to `"auto"`.
- Clearing `rate_scale` falls back to the config default (else `1.0`).
- Clearing `first_production_at` re-opens the append-only auto-mint.

Other responses: `400` invalid body / no override field; `404` account not found; `409`
plugin-virtual auth; `503` auth manager unavailable; `500` persist failure.

**Set example** — pin tier, throttle to half rate, backfill anchor:

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

**Clear example** — empty string or `null` clears; `first_production_at` omitted, so it is
left untouched:

```bash
curl -sS -X PATCH https://<host>/v0/management/auth-files/account-scheduling \
  -H "X-Management-Key: <management-key>" \
  -H "Content-Type: application/json" \
  -d '{ "name": "AC-14.json", "tier_override": "", "rate_scale": null }'
```

**Success `200`** — the `account_scheduling` value is the full refreshed projection
(§2.1–2.5):

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

Warm-up promotion is gated by health, not age alone: an account climbs the curve by
**age *and* health**. The effective warm-up stage is
`min(age-based stage, health-allowed stage cap)`; an early risk-control signal clamps the
cap down and holds it there until the account recovers. This applies to Claude accounts
only, consistent with this feature's overall scope (§1).

Key invariants:

- **Only ever lowers, never raises** (fail-safe): the gate can never push an account above
  the stage its age already earns.
- **Distress decelerates**: on a distress hit the cap drops by `demote-step` stages
  (floored at the strictest stage) and `warmup_last_distress_at` is stamped. Both the
  grading view (rpm / daily budget / concurrency) and the selection-weight view (freshness
  factor / maturity) see the lowered stage from one shared read-side clamp.
- **Recovery is slow and health-gated**: only a healthy *success*, with a cap present and
  at least `promote-cooldown-minutes` since the last distress, raises the cap by **one**
  stage; each step re-arms the cooldown, so at most one stage is regained per cooldown
  window. Once the cap reaches the age-deserved stage it is dropped entirely (pure age
  again). Promotion requires health + cooldown — an account cannot climb by simply aging
  while it keeps failing.
- **Mature accounts are out of scope** (never demoted): an account past the whole curve is
  governed by pure age; a mature account showing distress is already covered by cooldown /
  quota-deweight / auto-quarantine and is deliberately not pushed back into a warm-up
  daily-budget hard gate.
- **Cold accounts are skipped**: an un-anchored ("cold") account carries no cap.

Config (`account-scheduling.health-gate.*`; defaults are conservative fail-safe values —
design pins no exact numbers, pending calibration against real 201 data):

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Master switch. `false` = effective stage always equals the age stage (pre-ANCHOR-Q4 behavior). |
| `failure-cluster-threshold` | `3` | Failed requests within the observation window that mark distress. `0` disables this signal. |
| `backoff-level-threshold` | `1` | `Quota.BackoffLevel` (escalating plan-quota 429 backoff exponent) at or above which distress is marked. `1` = any active plan-quota backoff. `0` disables this signal. |
| `observation-window-minutes` | `30` | How far back the failure cluster is summed (only meaningful when `failure-cluster-threshold > 0`). |
| `demote-step` | `1` | Warm-up stages the cap drops per distress hit (must be `>= 1` when enabled). |
| `promote-cooldown-minutes` | `30` | Minimum time since the last distress before a healthy success may re-raise the cap by one stage (must be `> 0` when enabled). |

The two distress signals are **OR-ed** — either the failure cluster or the backoff level
alone marks distress. When `enabled`, config load requires at least one of the two
thresholds to be positive.

Operational reads (projection, §2.5): `in_distress` (currently signalling),
`warmup_health_stage_cap` (the persisted cap, `null` = none), and `warmup_last_distress_at`
(when it last decelerated). Compare `warmup.stage` (effective, capped) against
`warmup.age_days` (raw age) to spot an account that has been decelerated below its age.

Signal fidelity (v1): the gate uses only the two signals core already tracks —
recent-request failure clusters and `Quota.BackoffLevel` — and adds **no** precise 429
classification. Hard failures (quarantine / reauth / active cooldown) are not this layer's
concern; they are already filtered out of the selectable pool. The thresholds above are
conservative defaults awaiting calibration against real 201 traffic.

### 3.7 Reading the account page (cpamp management UI)

How the cpamp account page renders the §2.5 projection fields for operators:

- **Subscription-tier badge** (`20x` / `5x` / `Pro` / `unknown`): `subscription_tier`.
  `unknown` is expected, not a bug, when the upstream returns an unmapped `rate_limit_tier`
  (e.g. `default_claude_ai`) — pin a tier via `tier_override` (§3.1 / §3.5) if the account
  should participate in tier-weighted selection.
- **Warm-up badge**: `warmup.stage` (effective/capped stage) with `warmup.age_days`.
- **"Manual" marker**: `tier_source = "override"`.
- **Session counts**: `sessions_total` / `sessions_active` / `sessions_closed`.
- **Decelerated state**: `in_distress = true` (with `warmup_health_stage_cap` /
  `warmup_last_distress_at`) means the health gate has slowed this account down.

## 4. Caveats

- **New accounts are protected during warm-up**: new accounts (`"cold"` or an early stage
  within the curve) have their daily budget / RPM / concurrency pushed very low (the
  default curve's first stage is only 200/day, 3 RPM, concurrency 1), deliberately far
  below mature accounts, so that burst traffic naturally routes to mature accounts instead
  of concentrating on the newest, most fragile accounts.
- **Mature accounts have relaxed limits**: once an account passes the entire `warmup-curve`
  (age >= 60 days by default) it enters `mature-limits`: no more fixed daily budget, driven
  instead by quota headroom; RPM / concurrency / burst ceilings are also relaxed to a level
  that deliberately leaves headroom and only intercepts pathological bursts (not a level
  normal throughput would ever hit).
- **Rate-limit / daily-budget counters are rebuilt from in-memory, non-persistent state
  after a restart**: the per-account token bucket (`AccountRateLimiter`), the daily request
  counter, and the in-flight concurrency counter (`AccountConcurrencyGate`) all live purely
  in process memory — **none of it is written to disk or persisted**. After a process
  restart:
  - the token bucket starts fresh from a "full bucket" state (allowing the configured burst
    to be used up all at once);
  - the day's request counter and in-flight concurrency counter both reset to zero and
    start accumulating again.

  So the practical effect across a restart boundary is **biased toward being more
  permissive, not more conservative** — for example, if an account has already hit its
  `daily_budget` for the day, a restart zeroes that counter and the account effectively
  regains a fresh round of budget headroom for the rest of that day. This is a deliberately
  accepted "safe direction to err in" by design (better to occasionally allow a bit more
  across a restart boundary than to build a separate persistence subsystem for what is
  fundamentally short-lived counter state) — it is not a defect.
  - In contrast, the **`first_production_at` anchor is persisted** (written into the
    `metadata` of the account's auth JSON file) and is completely unaffected by a process
    restart — the age judgment that determines warm-up tier stays consistent across
    restarts; only the rate-limit / daily-budget / concurrency "counter" state gets
    rebuilt.

## Code index

Symbol/file locations, moved out of the prose above (verified against the current code as
of 2026-09):

| Mechanism / field | Code location |
| --- | --- |
| Selection weighting (`AccountSelectionWeight`) | `sdk/cliproxy/auth/account_weight.go` |
| Per-account token bucket (`AccountRateLimiter`) | `sdk/cliproxy/auth/account_rate_limiter.go` |
| Management-API projection write site | `internal/api/handlers/management/auth_files.go` (~line 490) |
| Legacy projection builder (`buildAdaptiveSchedulingView`) | `internal/api/handlers/management/auth_files_adaptive_scheduling.go` |
| Projection dual-emit site | `internal/api/handlers/management/auth_files.go` |
