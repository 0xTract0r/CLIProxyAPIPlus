# 自适应账号调度（Adaptive Account Scheduling）

> 实现对应 `openspec/changes/add-adaptive-account-scheduling`。config 字段定义已在
> `core/config.example.yaml` 的 `account-scheduling` 段完整覆盖，本文不重复列出，只补充
> 管理 API 的只读字段参考和运维说明。字段/行为已对照 2026-09 当前代码核实。

## 1. 概述

把 `routing.strategy` 设为 `adaptive` 即可启用这套选号策略（默认值仍是
`round-robin`，未设置 `adaptive` 时 `account-scheduling` 段完全不生效、被忽略）。启用后，
一次账号选择由以下几层机制共同决定：

- **按订阅等级容量 x 实时额度余量 x 新鲜度加权选号**：核心公式是
  `weight = tier 基础容量权重 x (1 - 额度利用率) x 新鲜度系数`
  （见 `sdk/cliproxy/auth/account_weight.go` 的 `AccountSelectionWeight`）。
  - tier 基础容量权重按 provider 分别配置，Claude 区分 `max_20x` / `max_5x` / `pro` /
    `unknown` 四档，Codex 区分 `pro` / `plus` / `unknown` 三档；两套权重只在各自 provider
    内部比较，从不跨 provider 比较（一个 Claude 权重和一个 Codex 权重没有可比性）。
  - 额度余量取账号"最紧张"的那个额度窗口（利用率最高的窗口），而不是任意窗口或平均值——
    一个 `five_hour` 窗口已经打到 90% 的账号，不会因为 `seven_day` 窗口还有余量就被当作
    安全可用。额度快照完全读不到时不当作"0% 已用"，而是取一个中性偏保守的兜底值。
  - 新鲜度系数由账号自 `first_production_at`（见 3.2 节）起的账龄，对照
    `account-scheduling.warmup-curve` 各阶段换算得出：曲线内账号系数 < 1，越过整条曲线
    的成熟账号系数为 1。
- **每账号限流平滑（非全局池）**：每个账号拥有独立的 token bucket（
  `sdk/cliproxy/auth/account_rate_limiter.go`），速率/突发容量取自该账号当前所在
  warm-up 阶段（或成熟态）的 `rpm-limit` / 并发上限。一个客户端打爆某个账号，不会挤压池
  子里其它账号的配额。当某个候选账号瞬时被限流時不会直接拒绝请求，而是继续在候选池里做
  加权抽取（"洪峰路由到成熟号"效果由权重 + token bucket 自然叠加得出，不需要单独的洪峰
  探测逻辑）。
- **新号养号期渐进放量**：新账号按 `warmup-curve` 配置的各阶段（默认 5 档，见
  `config.example.yaml`）逐周抬升 `daily-budget` / `rpm-limit` / `concurrency-limit`；
  越过最后一档（默认账龄 >= 60 天）后进入 `mature-limits`，不再受固定日预算约束，改为
  按额度余量驱动。
- **会话粘性分级**：`routing.session-affinity: true` 时，本选择器自带 session 粘性缓存，
  并按账号是否成熟做分级处理（而不是外层通用 session-affinity 包装器那种"命中就直接返回、
  完全不看内层选择器"的简单粘性）：
  - 粘性目标是**非 Claude/Codex 账号**（本调度器不打分的 provider）：不做分级，行为等同现
    有 session-affinity。
  - 粘性目标**成熟且未打到软上限**（token bucket 仍允许放行）：保持粘性，维持 prompt
    cache 连续性。
  - 粘性目标**成熟但已打到软上限**（视为"接近风控硬阈值"）：打破粘性，在全池重新加权选择。
  - 粘性目标**仍在养号期、且池子里存在可用的成熟账号**：打破粘性，改路由到成熟账号（并
    重新绑定，后续轮次跟随该成熟账号）。
  - 粘性目标**仍在养号期、且池子里没有任何成熟账号可选**：只要该养号账号本身仍可服务
    （未打满日预算/并发/token bucket），保持粘性，避免在全养号池里无意义地换号、白白丢失
    prompt cache；账号本身已不可服务时才会跨全池重选。
- **降级行为**：本调度器不识别的 provider（非 claude/codex），或某个等级被显式配置为
  权重 `0`，都会被排除出加权候选，回退到 `Fallback`（默认 `RoundRobinSelector`），与
  策略未启用时的行为一致，不影响这些 provider 的现有请求路径。

启用方式只需要改 `routing.strategy: "adaptive"`；`account-scheduling` 段（
`warmup-curve` / `mature-limits` / `tier-weights`）全部有内置默认值（源自一个真实生产
账号的养号轨迹观测，见 `config.example.yaml` 注释），按需覆盖即可，字段定义不在本文重复。

## 2. 管理 API 字段参考

`GET /v0/management/auth-files` 返回的每个账号对象里，新增一个只读、纯加性的
`adaptive_scheduling` 子对象（写入点：`internal/api/handlers/management/auth_files.go`
第 490 行；投影逻辑：`internal/api/handlers/management/auth_files_adaptive_scheduling.go`
的 `buildAdaptiveSchedulingView`）。

这是一个**纯读投影**：只读取账号记录上已经持久化的数据（`Metadata` 里的
`quota_snapshot` / `first_production_at`，`Attributes` 里的 `plan_type`）加上启动时加载
的养号曲线配置；不会铸造（mint）`first_production_at` 锚点，不会写回 auth 记录，也不会
触发任何上游请求。

未知状态一律显式表达为 JSON `null` 或 `"unknown"` 标签，绝不悄悄猜成一个具体值：缺失的
订阅等级、读不到的额度快照、尚未锚定的账号都不会被伪装成 `"pro"` / `"0% 已用"` / `"刚
出生"`，管理前端和农场编排器等调用方可以据此区分"还不知道"和"真实取值"。

### 2.1 `adaptive_scheduling.subscription_tier` (string)

账号的精细订阅等级：

- Claude 值域：`max_20x` / `max_5x` / `pro` / `unknown`。
- Codex 值域：`pro` / `plus` / `unknown`。
- provider 既非 `claude` 也非 `codex` 时，固定返回 Claude 一侧的 `unknown` 标签（两套
  枚举完全独立，不混用同一套值域）。

来源：Claude 读 `Metadata.quota_snapshot.profile.organization.rate_limit_tier`；Codex
读 `Attributes.plan_type`。两者都可以被顶层 `metadata.tier_override` 手动覆盖，见 3.1 节。

### 2.2 `adaptive_scheduling.quota_utilization` (object | null)

结构化的按额度窗口利用率。账号完全没有可用的 `quota_snapshot.usage` 快照时（从未探测
过 / 探测失败 / 该 provider 不轮询额度）整体为 `null`，绝不能读成"0% 已用"。

- `windows` (object)：key 是上游原始窗口名（如 `five_hour` / `seven_day` /
  `seven_day_sonnet`），value 是：
  - `utilization_percent` (number, 0-100)：上游原样的利用率百分比，已裁剪到 `[0,100]`。
  - `headroom` (number, 0-1)：`1 - utilization_percent/100`，已裁剪到 `[0,1]`。
  - `resets_at` (string, RFC3339 UTC，可选)：该窗口的重置时间；上游没给出可解析时间戳
    时字段不出现。
- `binding_window` (object，可选)：所有窗口里最紧张（`headroom` 最小）的那一个，即真正
  约束这个账号的窗口：
  - `window` (string)：绑定窗口名。
  - `headroom` (number, 0-1)。
  - `resets_at` (string, RFC3339 UTC，可选)。

Codex 的 `quota_snapshot.usage` 结构目前尚未在本仓库对真实生产账号抓包确认过；已知社区
逆向信息显示它可能把窗口嵌套在 `rate_limit.primary_window` / `secondary_window` 下、用
`percent_left` 而非顶层 `utilization` 字段表达——在这种情况下解析器大概率识别不到任何
窗口，`quota_utilization` 会正确显示为 `null`（"未知"），而不会把 `percent_left` 误读成
`utilization`。

### 2.3 `adaptive_scheduling.first_production_at` (string, RFC3339 UTC | null)

账号的新鲜度锚点。账号从未真正投产服务过（尚未锚定）时为 `null`。详见 3.2 节。

### 2.4 `adaptive_scheduling.warmup` (object)

账号当前所在的养号/限流阶段：

- `stage` (string)：阶段名。取值是 `account-scheduling.warmup-curve` 里配置的某个
  `name`（默认曲线为 `"w1"` / `"w2"` / `"w3-4"` / `"w5-6"` / `"w7-8"`），或两个合成态
  之一：`"cold"`（账号还没有 `first_production_at` 锚点，从未真正投产过）/ `"mature"`
  （账龄已越过整条 `warmup-curve`，套用 `mature-limits`）。
- `mature` (bool)：是否已越过整条养号曲线、套用 `mature-limits`。`"cold"` 和曲线内任意
  阶段都是 `false`。
- `freshness_factor` (number, 0-1)：该阶段的新鲜度系数**观测视图**——`cold` 固定为
  `0`，曲线内随账龄线性抬升（严格小于 `1`），`mature` 固定为 `1`。**注意**：这只是给运维
  观测用的独立视图，和实际参与选号加权计算的新鲜度系数不是同一份实现——选号侧
  （`AccountFreshnessWeightFactor`）对"尚未锚定"的账号返回 `1`（自举考虑：不能让一个还
  没机会投产的新账号永远陪跑最低权重、连锚定自己的机会都没有），而这个观测字段对同一
  种情况刻意返回最保守的 `0`（反封号兜底信号，"冷置"标签本身就是给人看的告警）。两者只在
  "尚未锚定"这一种情况上刻意分歧，其余情况数值语义一致。
- `daily_budget` (number)：该阶段允许的每 UTC 自然日请求数上限。`0` 表示不设日预算
  （`mature` 恒为 `0`：按额度打满为准，不再设固定日预算；曲线内某阶段本身配置为 `0`
  时同样表示该阶段不限）。
- `rpm_limit` (number)：该阶段的每分钟请求数限流上限（即 per-account token bucket 的
  补充速率）。
- `concurrency_limit` (number)：该阶段允许的最大同时在途请求数。
- `age_days` (number | null)：账号自 `first_production_at` 起的整数天龄。`"cold"`
  （未锚定）状态下为 `null`。

## 3. 运维说明

### 3.1 `tier_override` 手动标记

有些真实生产账号上游返回的 `rate_limit_tier` 是自动识别逻辑刻意不映射的值（例如
`default_claude_ai`——一个未收录的值绝不会被"猜"成某个已知档位），这类账号会一直落在
`subscription_tier = unknown`，无法参与按等级加权选号。运维可以手动"钉死"一个等级：

- 编辑该账号 auth JSON 文件，在顶层 `"metadata"` 对象里加一个字符串字段
  `"tier_override"`：
  - Claude 侧合法取值：`"max_20x"` / `"max_5x"` / `"pro"`。
  - Codex 侧合法取值：`"codex_pro"` / `"codex_plus"`（用 `codex_` 前缀和 Claude 的
    `"pro"` 区分，避免同一个 key 在两个 provider 下语义冲突）。
  - 取值不区分大小写、自动去除首尾空白；空值、非法值、或跨 provider 的值（比如给 Codex
    账号写了 Claude 的 `"max_20x"`）都会被忽略，自动回退到原有的自动识别路径——现有行为
    在没有合法覆盖时完全不受影响。
- 该 key 是**顶层** `metadata` 字段，不是嵌套在 `quota_snapshot` 内部——这是刻意设计：
  额度轮询刷新时会整体替换 `quota_snapshot` 子对象，写进 `quota_snapshot` 内部的值会在
  下一次刷新（约 45 分钟一轮）被覆盖冲掉，写在顶层则不受影响。
- 当前没有专门的管理写入端点来设置这个字段，只能直接编辑账号的 auth JSON 文件。
- 典型用途：自动识别不准（如上述 `default_claude_ai` 场景）、或需要人为模拟/测试某个
  等级的加权选号行为时使用。

### 3.2 `first_production_at` 锚点

这是账号新鲜度（养号年龄）的唯一锚点，决定它落在 `warmup-curve` 的哪一档、以及新鲜度
加权系数。

- **是什么**：这个账号第一次成功服务一次真实请求的墙钟时间，一次性打上时间戳、
  此后永不改写。
- **何时铸造**：由后续调用方（选号/执行路径）在账号第一次成功服务真实请求时调用
  `EnsureAuthFirstProductionAt` 铸造并持久化——失败的请求、`count_tokens` 预检请求、
  ephemeral Home dispatch 都刻意不会触发铸造，只有真正成功的一次实际服务才算数；本文
  2.3/2.4 节描述的管理 API 投影本身是纯读的，绝不会主动铸造这个锚点。
- **append-only，不被额度刷新覆盖**：这个字段刻意**不是**账号文件的 mtime 或
  `CreatedAt`——两者都会被无关的 token/额度刷新写入、或者一次 re-auth（会替换底层文件）
  悄悄改动，都不足以在账号整个生命周期内保持稳定。`first_production_at` 存在同一个
  `Metadata` map 里（和 `quota_snapshot` / `rate_limit_tier` 相邻），但只在从未设置过、
  或已存值损坏无法解析时才会被（重新）写入一次；已有的合法值任何后续调用都不会覆盖。
- **对养号档位的影响**：账号年龄 = 当前时间 - 这个锚点，取整数天数后对照
  `warmup-curve` 各阶段的 `[min-age-days, max-age-days)` 区间查表；越过最后一档进入
  `mature-limits`。**没有**这个锚点的账号（`"cold"` 态）会被限制在曲线**第一档**（也就是
  最严格的一档）的限流阈值下，但选号权重侧的新鲜度系数按 `1` 处理（见 2.4 节的分歧说明），
  以便这类账号仍有机会赢得一次选中、从而完成自己的锚定。

## 4. 注意事项

- **养号期新号受保护**：新账号（`"cold"` 或曲线内早期阶段）的日预算/RPM/并发都被压得很
  低（默认曲线第一档仅 200/日、3 RPM、并发 1），刻意远低于成熟号，目的是把突发流量自然
  路由到成熟账号，而不是集中打在刚上线、最脆弱的新账号上。
- **成熟号放开限制**：账号越过整条 `warmup-curve`（默认账龄 >= 60 天）后进入
  `mature-limits`：不再设固定日预算，改为按额度余量驱动；RPM/并发/突发上限也放宽到一个
  刻意留有余量、只拦截病态突发流量的水位（不是日常吞吐会碰到的水位）。
- **重启后限流/日预算计数从内存态、非持久态重建**：per-account token bucket
  （`AccountRateLimiter`）、每日请求计数与并发在途计数（`AccountConcurrencyGate`）全部
  只存在进程内存里，**不落盘、不持久**。进程重启后：
  - token bucket 重新从"满桶"状态起步（允许一次性把配置的 burst 用满）；
  - 当日请求计数、并发在途计数都清零重新累计。
  这意味着重启前后的实际效果是**偏宽松而不是偏保守**——比如某账号当天已经打满
  `daily_budget`，进程重启会让这个计数清零，该账号当天实质上重新获得了一轮配额余量；这是
  设计上刻意接受的"安全出错方向"（宁可跨重启边界稍微多放一点，也不为这套本质上短生命周期
  的计数状态另起一套持久化子系统），不是缺陷。
  - 与此相对，**`first_production_at` 锚点是持久化的**（写在账号 auth JSON 文件的
    `metadata` 里），完全不受进程重启影响——决定养号档位的账龄判断在重启前后保持一致，
    只有限流/日预算/并发这些"计数器"状态会重建。
