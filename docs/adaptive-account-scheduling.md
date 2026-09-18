# Adaptive Account Scheduling

[简体中文](adaptive-account-scheduling_CN.md)

> Implements `openspec/changes/add-adaptive-account-scheduling`. Config field definitions
> are already fully covered in the `account-scheduling` section of
> `core/config.example.yaml` and are not repeated here; this document only adds the
> management API read-only field reference, operational notes and the serving-reserve
> extensions (`add-warmup-serving-reserve`, `add-warmup-traffic-pacing`). Fields/behavior have
> been verified against the current code as of 2026-09. Code/symbol locations are
> consolidated in the [Code index](#code-index) at the end, not inlined after each claim.

## Operator quick reference

Common operator tasks and where each one lives:

| I want to… | Use | Where |
| --- | --- | --- |
| Turn adaptive scheduling on | `routing.strategy: "adaptive"` | §1 |
| Bound warming send bursts and admission | `warmup-traffic-pacing` | [Traffic pacing](#warm-up-traffic-pacing) |
| Enable warming opportunities and check production values | `warmup-serving-reserve` and migration controls | [Serving reserve](#opt-in-warm-up-serving-reserve) |
| Read an account's tier / quota / warm-up state | `GET /v0/management/auth-files` → the `account_scheduling` projection | §2 |
| Pin a subscription tier on one account | `tier_override` (endpoint or auth JSON) | §3.1 / §3.5 |
| Migrate an old account / backfill its warm-up anchor | `first_production_at` (endpoint backfill) | §3.2 / §3.5 |
| Slow-speed safety-test one account | `rate_scale` (`< 1` throttles) | §3.4 / §3.5 |
| See why one account was decelerated | `in_distress` / `warmup_health_stage_cap` / `warmup_last_distress_at`; compare `warmup.stage` vs `warmup.age_days` | §3.6 / §3.7 |
| Change an operator override (tier / rate / anchor) | `PATCH /v0/management/auth-files/account-scheduling` | §3.5 |

## Recommended values quick reference

> These recommended values used to be buried in the ops deployment doc, out of sight when
> reading the parameters; this section lifts them into the parameter reference so you can
> see what to set right here. **The authoritative source for deployment-time configuration
> remains `docs/operations/deploy/remote-core-maintenance.md`** (in the umbrella
> `cliproxy-stack` repo); this table is only a quick reference and that doc wins on any
> discrepancy.

| Parameter | Default | Recommended | Notes |
| --- | --- | --- | --- |
| `account-scheduling.warmup-traffic-pacing.enabled` | `false` | Explicit opt-in after isolated validation | Independent of serving reserve; see [Traffic pacing](#warm-up-traffic-pacing). |
| `account-scheduling.warmup-serving-reserve` | `0` (off) | `0.15` (this production profile) | Shared new-opportunity probability, not a total traffic cap; explicitly persist it to enable. See [Serving reserve](#opt-in-warm-up-serving-reserve). |
| `account-scheduling.warmup-serving-max-binding-age-seconds` | `0` | `0` (age fallback off) | Non-renewing age in seconds; a positive value alone does not enable migration. |
| `account-scheduling.warmup-serving-migration-token-budget` | `0` | `0` (proactive migration off) | Per-process rolling-hour estimated input reconstruction budget, separate from daily account tokens. |
| `account-scheduling.rate-scale` / per-account `rate_scale` (§3.4) | `1.0` | `1.0` (no scaling) | Effective rate-limit multiplier (scales rpm/burst/concurrency/daily-budget). **There is no "set X for old accounts / Y for new accounts" scenario number** — the default is simply `1.0`; only set a specific account `< 1` for a low-risk slow test, tuning as needed. Do not copy a fixed number. Must be `> 0`. |
| `account-scheduling.anti-streak-limit` (anti-streak) | `0` (off) | **`3`** (production) | Force-rotate a **warm-up account** away once it has been picked ≤ this many times in a row. Applies only to warm-up accounts (mature accounts are exempt) and is cache-safe (does not change the long-term 20:5:1 share); largely idle when a single mature account carries all traffic (no warm-up accounts to rotate to). |
| `account-scheduling.warmup-curve[*].token-daily-budget` (warm-up account token daily budget) | `0` (unbounded) | **`0` — pending real-traffic calibration** | Rolling-24h billable-token hard gate for warm-up accounts. **No concrete positive value has been calibrated yet**: run a real-traffic gray release to see the burn curve first, then set it — **do not just pick a number**. Mature accounts' `mature-limits.token-daily-budget` stays `0` (unbounded). |
| `account-scheduling.tier-weights.claude` | `max_20x:20 / max_5x:5 / pro:1 / unknown:1` | Same as default (20:5:1) | Weighted selection by Claude subscription tier. |
| `max-retry-credentials` (top-level, not inside `account-scheduling`) | `2` | `2` | How many accounts a single failed request may fail over across. `-1` = escape hatch (traverse the whole pool; use with care). |
| warm-up curve (`warmup-curve` per-stage rpm / concurrency / daily-budget) | see stage table | see stage table | The rpm / concurrency / daily-budget of each warm-up stage (cold / w1 / w2 / w3-4 / w5-6 / w7-8 / mature) live in the default curve referenced by §1 "Progressive ramp-up during a new account's warm-up period", or the warm-up stage table in `docs/operations/deploy/remote-core-maintenance.md` — not repeated here. |

Honest caveats:

- Everything above is a **global config-file knob** (the `account-scheduling` section of the
  remote `config.yaml` plus the top-level `max-retry-credentials`) and is **not editable from
  the settings-page UI**. The settings-page UI only exposes three per-account controls:
  `tier` (subscription tier, §3.1), `rate_scale` (rate multiplier, §3.4), and the
  first-production anchor `first_production_at` (§3.2).
- **The authoritative source for deployment-time configuration is
  `docs/operations/deploy/remote-core-maintenance.md`** (in the umbrella `cliproxy-stack`
  repo); this table is a quick reference and that doc is the source of truth.

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
  not minted by failures, ephemeral Home dispatch, or the read-only §2.3/§2.4 projection. Opt-in Claude warming CountTokens also excludes anchor minting. Default-off and other count paths retain legacy MarkResult handling; the previous blanket statement that all count preflights were excluded was inaccurate.
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
- **Restart behavior differs by state:** the per-account RPM token bucket and in-flight concurrency counters are process-local and restart from their initial state. The warming request budget instead uses a rolling 24-hour window of hourly buckets, persisted in auth metadata as `account_scheduling.daily_budget_window`; a cold gate restores those saved buckets. It does not intentionally reset the day's request budget to zero on restart.
- With a positive daily token budget, completed usage is recorded in `account_scheduling.token_budget_window` and restored similarly. A zero token budget leaves that protection and its recording disabled. Ordinary results use `MarkResult` and tokens use the usage sink. Opt-in Claude warming execution additionally reserves before sending and counts sent cancellations neutrally; completed persisted records still do not guarantee that unfinished requests survive a crash.
- The `first_production_at` anchor is also persisted in auth metadata and is not reset by a process restart. See `account_gate.go` and `conductor_cooldown.go` for budget-window persistence and restoration.

## Opt-in warm-up serving reserve

All three fields belong to `account-scheduling` and default to zero. The disabled default preserves existing routing for unconfigured installations and upgrades; **omitting a field does not automatically select the production recommendation**.

| Parameter | Meaning | Default | This production profile |
| --- | --- | ---: | ---: |
| `warmup-serving-reserve` | Probability of reserving a new independent service opportunity for warming accounts; finite `0 <= value < 1` | 0 (disabled) | 0.15 |
| `warmup-serving-max-binding-age-seconds` | Non-renewing binding-age threshold in seconds; also controls the underserved observation period | 0 (age fallback disabled) | 0 |
| `warmup-serving-migration-token-budget` | Per-process rolling-hour estimate budget for input-cache reconstruction when migrating existing bindings | 0 (all proactive migration disabled) | 0 |

The production values were explicitly persisted on 2026-09-15; they are environment configuration, not built-in defaults. To enable the same policy in a new environment, merge these fields into the corresponding sections of the existing configuration rather than replacing the entire file:

```yaml
routing:
  strategy: adaptive
  session-affinity: true
account-scheduling:
  warmup-serving-reserve: 0.15                    # Enable the 15% new-opportunity reserve
  warmup-serving-max-binding-age-seconds: 0      # Keep age fallback disabled
  warmup-serving-migration-token-budget: 0       # Keep proactive migration disabled
```

**Avoid missing configuration:** the generic `config.example.yaml` remains disabled by default. When creating or restoring a production configuration, check these three fields plus adaptive routing and session affinity. A persisted 0.15 survives a normal restart; deployment must preserve the existing runtime configuration instead of overwriting it with template zeros. Verify the actual configuration, not merely the example file or deployed feature version.

A positive reserve adds opportunities for eligible new Claude sessions and reliably independent children. Warming accounts share the probability; it is not a request/token share or a finite-sample minimum. A qualified fresh selection with a mature fallback receives continuity protection whether it came from the reserve draw or ordinary weighting. Logs distinguish `reserve`, `weighted`, `migration`, and `inherited`; weighted selection is not counted as a reserve win.

A protected child normally stays on its account. Temporary RPM, concurrency, or pending-request-budget pressure can wait outside scheduler locks, for at most **30 seconds accumulated across the logical request**, with one waiter per account and 64 per process. The caller's shorter deadline wins. Each wake-up rechecks current state; execution waiting returns through normal selection and preparation before sending, so stale credentials or model permissions are not reused.

| Condition | Result |
| --- | --- |
| Healthy original account has capacity | Continue on the same account; reuse an already charged token for this request |
| Original account has an in-flight request, or the same binding already has a waiter | An overlapping request may borrow an available mature account without changing the original binding, summary, pin or TTL |
| Serial RPM pressure | Wait within the bounds without renewing the binding, spending another token, or counting a request |
| Overlap without an available mature account | Return retryable busy while preserving the valid original binding |
| Wait expires or the queue is full outside the borrowing case | Commit a stable handover only after selecting an available mature account |
| No mature fallback after the wait | Return retryable busy; retain a still-valid binding without extending its TTL |
| Budget exhausted, disabled/quarantined/removed account, or real upstream failure | Apply safe failover; do not create another reserve draw |
| Client cancels while waiting | Release the waiter; do not send a background model request |

Borrowing handles only the overlapping request; the original execution/waiter and later serial turns retain their binding. It does not classify progress-description prompt text. A committed handover does not automatically bounce back when the warming token recovers. This avoids repeatedly rebuilding the same prompt cache. Existing proactive migration still needs its separately configured budget and eligibility below. The fixed wait bounds add no configuration fields.

Fresh-child recognition requires a reliable parent, an independent first text task, and no fork or inherited-history evidence. The remaining messages may contain complete CLI tool catalogs, skill catalogs, and/or the exact `Today's date is YYYY-MM-DD.` notice. A catalog does **not** require a date. Complete known units may be reordered or packaged across text blocks/system messages; a heading and its entries must remain together. Auxiliary input is bounded to 128 KiB and eight messages/text blocks. Empty or duplicate catalogs, malformed dates, unknown independent paragraphs, split units, media and unknown blocks remain conservative.

Descriptions are opaque text, including observed unindented continuation lines within a skill entry; this grammar is not a provenance or semantic security check. Parent and child catalogs need not match. A separate bounded normalized task hash and byte length reject full parent-task reuse and exact prefix extensions, without mistaking a shared short or large context prefix for the complete parent task. This does not recognize arbitrary paraphrases or prepended wrappers. Tasks above 256 KiB remain conservative. Summaries store hashes and sizes, never prompt text.

Recognition never rewrites payloads or headers, and leaves existing system/tools/first hashes, message counts, input cost, cache TTL and migration eligibility unchanged. Parent history accepts supported text system notices, direct tool callers and tool references in their defined positions; tool argument JSON remains data. Existing children/resumes are checked before fresh classification. Unknown/fork children prefer an available parent account; nested children use `x-claude-code-parent-agent-id`, and identities remain separated by provider, root, agent and model. A child does not overwrite its parent binding. A heterogeneous provider pool retains its existing policy.

With reserve enabled, adaptive Claude warming executions reserve concurrency and remaining request budget **before** sending, including count preflights and stream startup/retry paths. Pending reservations count toward admission and remain held through result accounting. The existing result unit is preserved: an internal 401 refresh/resend shares its final result's count; this is not a per-HTTP-attempt global pacing guarantee. Unsent cancellation counts zero; a sent execution cancelled before any result contributes one neutral request-budget entry without account failure, health demotion or a first-production anchor. A silent upstream cannot retain the slot after cancellation. These reservations are local to the process, not crash-proof accounting.

Opt-in count preflights use ordinary selection/RPM/error accounting, but do not create or overwrite serving bindings, clear pins, draw reserve opportunities or mint a warming account's first-production anchor. The existing unsupported-count-endpoint 404 exception remains neutral. Default-off and non-target count paths retain their legacy result handling; the anchor exclusion is not a claim that all legacy count paths were changed.

Wait starts and terminal outcomes are logged at Info with request/account correlation and elapsed wait, without request text or credentials. Binding hits are not prompt-cache hits: use the upstream cache-read/cache-creation usage fields for cache evidence.

With reserve enabled and a positive migration budget, **existing bindings** can be reassessed after cache-expiry idle windows or substantial verified context shortening. A positive maximum binding age additionally enables age-based reassessment; turns do not renew that age. Requests need a conservative text-input cost estimate, a source account without in-flight requests, and a warming target whose real outbound count has not advanced throughout the observation period. That period uses the configured binding age, capped at 24 hours, or one hour when age fallback is disabled. Recently assigned targets wait the same period. Selection counts remain separate from actual outbound attempts and never count as successes.

The migration budget is an **estimate** of input reconstruction tokens, not an account's daily token budget; rolling-hour credit is reserved atomically before selection. Opaque/media input, insufficient budget, no underserved target or an in-flight source suppresses migration. The source check is an instantaneous account-level observation, not a distributed session lock. A cached assistant prefix followed by an uncached user task also suppresses migration to protect the observed compaction request shape; this is deliberately broader than compaction and does not identify every implementation.

Unknown cache TTLs conservatively use one hour. Idle reassessment needs a surviving session binding, so the affinity TTL should exceed the cache TTL. Expired bindings retain a bounded one-hour marker that suppresses an additional reserve lottery and uses ordinary reselection. Cache entries/markers are capped at 4096, migration charges are likewise bounded, and serving reserve alone introduces no persistence store; independent pacing uses the sidecar described below.

Setting reserve to zero stops reserve opportunities. When pacing remains enabled, keep its identities, groups, waiting and overlap protection. Turning both controls off clears child bindings, pins and migration state on config commit; ordinary root bindings and legacy D5 remain. Invalidation/expiry markers still suppress extra reserve draws without renewal. Count and other-provider traffic do not clear Claude state. Independent hard traffic pacing is described below.

## Warm-up traffic pacing

`account-scheduling.warmup-traffic-pacing` independently controls warming-account send cadence and defaults to disabled. It applies to the native adaptive Claude path, including eligible Claude-only mixed pools. A positive serving reserve does not enable pacing.

| Field | Default | Meaning |
| --- | ---: | --- |
| `enabled` | `false` | Enable the additional pacing and admission constraints |
| `request-burst` | `8` | Maximum stored send credits, including unsent reservations; not concurrency |
| `min-admission-requests` | `4` | Minimum credits to admit a new independent conversation |
| `max-active-bindings` | `1` | Maximum recently active independent conversation groups per account |
| `active-binding-idle-seconds` | `300` | Group inactivity expiry, independent of prompt-cache TTL |

When enabled, integer limits must be positive and the admission minimum cannot exceed capacity. A new ledger starts at zero. Credits refill continuously at the effective daily request budget divided by 86400 seconds. At 200 requests/day, one credit takes 7.2 minutes, admission at four takes about 28.8 minutes, and eight idle credits take about 57.6 minutes. The service does not generate traffic to fill a quota.

Admitted conversations prefer their current account, but every send still observes credit, rolling-60-second and rolling-24-hour budgets, concurrency and health. Temporary RPM/concurrency pressure uses the existing request-total 30-second wait. Overlap can borrow a mature account without replacing the primary binding. Long-term credit/budget/admission exhaustion uses a stable mature handoff; recovering one credit does not reclaim that conversation. Without an eligible fallback, return retryable capacity pressure.

Fresh children use independent groups; reliable parent-affine fork/unknown children share the known parent group while still spending per-account allowance. Releasing one parent/child/alias membership does not free another active member. CountTokens consumes request/concurrency allowance without creating or renewing groups or stamping first production; input estimation is not generation usage.

Each explicit HTTP retry is separately admitted. Unsent cancellation refunds its reservation; sent failures and disconnected streams remain counted. Positive token budgets include a conservative estimate of the final request, and only complete terminal usage permits reconciliation refunds. Scheduler accounting excludes cache reads and retains cache writes. The estimator recognizes the known `clear_thinking_20251015` context edit (default retention, `keep: "all"`, or a positive `thinking_turns` count) and still counts the complete unedited body. Unknown edits, mixed generation/compaction edits and unsupported content remain unestimated; positive token budgets reject those warming sends. See [Claude context editing](https://platform.claude.com/docs/en/build-with-claude/context-editing). A zero token budget leaves that protection disabled and provides no subscription-quota safety guarantee.

Independent `.pacing` files live in the durable auth directory and do not depend on usage-reporting enablement. Known previous consumption is combined with new attempts; earlier in-flight work cannot let the new policy spend the same remaining allowance. Restart preserves debits while releasing dead-process concurrency. Corrupt state or pre-send save failure blocks warming sends. A post-send settlement failure preserves the current response, emits a diagnostic and blocks later sends for that account. Deleting the ledger is not a quota-recovery procedure.

Legacy usage lacks a trustworthy completeness marker. Unknown history can block a positive token budget until its conservative window expires; legacy hourly aggregates may remain until 24 hours after that hour ends. Ambiguous cross-file crash recovery can overcount conservatively and is not exactly-once accounting. Protection is per instance; independent instances cannot safely spend separate local allowances for the same account.

Setting reserve to zero stops reserve opportunities. While pacing remains enabled, retain the identities, groups, waiting and overlap state it needs. Disabling pacing restores the legacy policy while retaining accounting for re-enable; disabling both controls restores the complete legacy-off behavior. Persist an explicit enabled configuration when rolling out: deploying code alone does not enable pacing.

Anonymous content fingerprints (`msg:` / SDK-derived identities) and conflicting identities are not reusable pacing groups; use a reliable session identity or a mature fallback. Custom native-Claude SDK executors must implement `HTTPAttemptGateAware` and honor the attempt hook on every send; otherwise warming execution fails closed when pacing is enabled. Logs `warmup-pacing-sent`, `warmup-pacing-denied` and `warmup-pacing-settled` expose balances, minute/day attempts, pending tokens and estimate/settlement differences without request bodies or credentials.

## Code index

Symbol/file locations, moved out of the prose above (verified against the current code as
of 2026-09):

| Mechanism / field | Code location |
| --- | --- |
| Durable pacing, hourly history bridge and sidecar | `sdk/cliproxy/auth/warmup_pacing.go`, `warmup_pacing_history.go`, `warmup_pacing_store.go` |
| Manager-owned pacing, executor lifecycle and final send admission | `sdk/cliproxy/auth/warmup_pacing_manager.go`, `warmup_pacing_calls.go`, `warmup_pacing_execution.go` |
| Selector pacing and group membership | `sdk/cliproxy/auth/warmup_pacing_selector.go` |
| Claude HTTP attempt hook / terminal usage observer | `sdk/cliproxy/executor/http_attempt.go`, `internal/runtime/executor/helps/claude_attempt*.go` |
| Selection weighting (`AccountSelectionWeight`) | `sdk/cliproxy/auth/account_weight.go` |
| Opt-in service reserve, child identity and migration | `sdk/cliproxy/auth/warmup_serving.go` |
| Auxiliary notices and full-task fingerprints | `sdk/cliproxy/auth/warmup_serving_auxiliary.go`, `warmup_serving_task.go` |
| Bounded waiting and execution admission/settlement | `sdk/cliproxy/auth/warmup_serving_wait.go`, `warmup_execution.go` |
| Per-account token bucket (`AccountRateLimiter`) | `sdk/cliproxy/auth/account_rate_limiter.go` |
| Management-API projection write site | `internal/api/handlers/management/auth_files.go` (~line 490) |
| Legacy projection builder (`buildAdaptiveSchedulingView`) | `internal/api/handlers/management/auth_files_adaptive_scheduling.go` |
| Projection dual-emit site | `internal/api/handlers/management/auth_files.go` |
