# 自适应账号调度（Adaptive Account Scheduling）

> 实现对应 `openspec/changes/add-adaptive-account-scheduling`。config 字段定义已在
> `core/config.example.yaml` 的 `account-scheduling` 段完整覆盖，本文不重复列出，只补充
> 管理 API 的只读字段参考和运维说明。字段/行为已对照 2026-09 当前代码核实。代码/符号位置
> 统一收敛在文末[代码索引](#代码索引)，不再逐句内联在每个论断后面。

## 运维速查

常见运维任务与对应入口：

| 我想… | 用什么 | 在哪 |
| --- | --- | --- |
| 启用自适应调度 | `routing.strategy: "adaptive"` | §1 |
| 读某号的订阅档 / 额度 / 养号态 | `GET /v0/management/auth-files` 的 `account_scheduling` 投影 | §2 |
| 给某号钉死订阅档 | `tier_override`（端点或 auth JSON） | §3.1 / §3.5 |
| 迁移老号 / 回填养号锚点 | `first_production_at`（端点回填） | §3.2 / §3.5 |
| 慢速安全测试某号 | `rate_scale`（`< 1` 限速） | §3.4 / §3.5 |
| 查某号为啥被减速 | `in_distress` / `warmup_health_stage_cap` / `warmup_last_distress_at`；对比 `warmup.stage` vs `warmup.age_days` | §3.6 / §3.7 |
| 改运维覆盖（档位 / 速率 / 锚点） | `PATCH /v0/management/auth-files/account-scheduling` | §3.5 |

## 1. 概述

> **适用范围：仅 Claude 账号。** 本特性——调度投影、加权选号、养号、下文的管理端点——只
> 作用于 Claude 账号。非 Claude 账号（Codex / Gemini / xAI 等）完全不受影响：
> `GET /v0/management/auth-files` 不会给它们返回 `account_scheduling` 投影对象。

自适应调度决定*每次请求由哪个账号服务*：按订阅容量、实时额度、新鲜度给账号打分选号，
而不是简单轮询（round-robin）。

**怎么启用。** 把 `routing.strategy` 设为 `adaptive`（默认值仍是 `round-robin`）。未设置
时，整个 `account-scheduling` 段完全不生效、被忽略。该段（`warmup-curve` / `mature-limits`
/ `tier-weights`）全部有内置默认值（源自一个真实生产账号的养号轨迹观测，见
`config.example.yaml` 注释），按需覆盖即可。字段定义在 `config.example.yaml`，不在本文重复。

启用后，一次账号选择由以下几层机制共同决定。

### 加权选号：订阅等级容量 x 实时额度余量 x 新鲜度

核心公式：

```text
weight = tier 基础容量权重 x (1 - 额度利用率) x 新鲜度系数
```

- **tier 基础容量权重**按 Claude 订阅档配置：
  - Claude 区分四档：`max_20x` / `max_5x` / `pro` / `unknown`。
- **额度余量**取账号最紧张的那个额度窗口（利用率最高），不取任意窗口或平均值。
  - 例：`five_hour` 窗口已经打到 90% 的号，即便 `seven_day` 窗口还有余量，也不算安全可用。
  - 额度快照完全读不到时：取一个中性偏保守的兜底值，绝不当作"0% 已用"。
- **新鲜度系数**由账号自 `first_production_at`（§3.2）起的账龄，对照
  `account-scheduling.warmup-curve` 各阶段换算得出。
  - 曲线内账号系数 `< 1`。
  - 越过整条曲线的成熟账号系数为 `1`。

### 每账号限流平滑（非全局池）

每个账号拥有独立的 token bucket，所以一个客户端打爆某个账号，不会挤压池子里其它账号的配额。

- 速率/突发容量取自该账号当前所在 warm-up 阶段（或成熟态）的 `rpm-limit` / 并发上限。
- 某个候选账号瞬时被限流时不会被直接拒绝，而是继续在剩余候选池里做加权抽取。
- "洪峰路由到成熟号"效果由权重 + token bucket 自然叠加得出，不需要单独的洪峰探测逻辑。

### 新号养号期渐进放量

新账号的 `daily-budget` / `rpm-limit` / `concurrency-limit` 逐周抬升。

- 放量按 `warmup-curve` 配置的各阶段进行（默认 5 档，见 `config.example.yaml`）。
- 越过最后一档（默认账龄 >= 60 天）后进入 `mature-limits`：不再设固定日预算，改为按额度
  余量驱动。

### 会话粘性分级

`routing.session-affinity: true` 时，本选择器自带 session 粘性缓存，并按账号是否成熟做分级
处理——而不是外层通用 session-affinity 包装器那种"命中就直接返回、完全不看内层选择器"的
简单粘性。

| 粘性目标状态 | 行为 |
| --- | --- |
| **非 Claude 账号**（本调度器不打分的 provider） | 不做分级，行为等同现有 session-affinity。 |
| **成熟且未打到软上限**（token bucket 仍允许放行） | 保持粘性，维持 prompt cache 连续性。 |
| **成熟但已打到软上限**（视为接近风控硬阈值） | 打破粘性，在全池重新加权选择。 |
| **仍在养号期、且池子里存在可用的成熟账号** | 打破粘性，改路由到成熟账号并重新绑定（后续轮次跟随该成熟账号）。 |
| **仍在养号期、且池子里没有任何成熟账号可选** | 只要该养号账号本身仍可服务（未打满日预算/并发/token bucket），就保持粘性——避免在全养号池里无意义换号、白白丢 prompt cache。账号本身已不可服务时才跨全池重选。 |

### 降级行为

本调度器不识别的 provider（非 claude），或某个等级被显式配置为权重 `0`，都会被排除出
加权候选，回退到 `Fallback`（默认 `RoundRobinSelector`）——与策略未启用时的行为一致，不
影响这些 provider 的现有请求路径。

## 2. 管理 API 字段参考

`GET /v0/management/auth-files` 返回的每个账号对象里，现在都带一个只读、纯加性的调度投影。

这是一个**纯读投影**：

- 只读取账号记录上已经持久化的数据——`Metadata.quota_snapshot` /
  `Metadata.first_production_at`、`Attributes.plan_type`——加上启动时加载的养号曲线配置。
- 不会铸造（mint）`first_production_at` 锚点，不会写回 auth 记录，也不会触发任何上游请求。

未知状态一律显式表达、绝不悄悄猜成一个具体值：

- 用 JSON `null` 或 `"unknown"` 标签表达。
- 缺失的订阅等级、读不到的额度快照、尚未锚定的账号，都不会被伪装成 `"pro"` / `"0% 已用"` /
  `"刚出生"`。
- 管理前端、农场编排器等调用方可以据此区分"还不知道"和"真实取值"。

### 2.1 `subscription_tier` (string)

账号的精细订阅等级。

- 该字段仅对 Claude 账号存在。
- 值域：`max_20x` / `max_5x` / `pro` / `unknown`。
- 来源：`Metadata.quota_snapshot.profile.organization.rate_limit_tier`。可以被顶层
  `metadata.tier_override` 手动覆盖（§3.1）。

### 2.2 `quota_utilization` (object | null)

结构化的按额度窗口利用率。

账号完全没有可用的 `quota_snapshot.usage` 快照时（从未探测过 / 探测失败 / 该 provider 不
轮询额度）整体为 `null`，绝不能读成"0% 已用"。

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `windows` | object | key 是上游原始窗口名（如 `five_hour` / `seven_day` / `seven_day_sonnet`）。 |
| `windows.<w>.utilization_percent` | number, 0-100 | 上游原样的利用率百分比，已裁剪到 `[0,100]`。 |
| `windows.<w>.headroom` | number, 0-1 | `1 - utilization_percent/100`，已裁剪到 `[0,1]`。 |
| `windows.<w>.resets_at` | string, RFC3339 UTC，可选 | 该窗口的重置时间；上游没给出可解析时间戳时字段不出现。 |
| `binding_window` | object，可选 | 所有窗口里 `headroom` 最小（最紧张）的那一个——即真正约束这个账号的窗口。 |
| `binding_window.window` | string | 绑定窗口名。 |
| `binding_window.headroom` | number, 0-1 | |
| `binding_window.resets_at` | string, RFC3339 UTC，可选 | |

### 2.3 `first_production_at` (string, RFC3339 UTC | null)

账号的新鲜度锚点。

- 账号从未真正投产服务过（尚未锚定）时为 `null`。
- 详见 §3.2。

### 2.4 `warmup` (object)

账号当前所在的养号 / 限流阶段。

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `stage` | string | 当前阶段名：`warmup-curve` 里某个 `name`（默认曲线 `"w1"` / `"w2"` / `"w3-4"` / `"w5-6"` / `"w7-8"`），或一个合成态——`"cold"`（还没有 `first_production_at` 锚点，从未真正投产）/ `"mature"`（账龄已越过整条曲线，套用 `mature-limits`）。 |
| `mature` | bool | 是否已越过整条曲线、套用 `mature-limits`。`"cold"` 和曲线内任意阶段都是 `false`。 |
| `freshness_factor` | number, 0-1 | 该阶段新鲜度系数的**观测视图**（见下方分歧说明）。`cold` = `0`；曲线内随账龄线性抬升（严格 `< 1`）；`mature` = `1`。 |
| `daily_budget` | number | 该阶段每 UTC 自然日请求数上限。`0` = 不设日预算（`mature` 恒为 `0`，按额度余量驱动；曲线内某阶段本身也可配置为 `0` = 该阶段不限）。 |
| `rpm_limit` | number | 该阶段每分钟请求数限流上限（即 per-account token bucket 的补充速率）。 |
| `concurrency_limit` | number | 该阶段允许的最大同时在途请求数。 |
| `age_days` | number \| null | 账号自 `first_production_at` 起的整数天龄。`"cold"`（未锚定）状态下为 `null`。 |

**`freshness_factor` 分歧说明。** 这个字段是给运维观测用的独立视图，和实际参与选号加权
计算的新鲜度系数不是同一份实现。两者只在一种情况上刻意分歧——账号"尚未锚定"：

- 选号侧（`AccountFreshnessWeightFactor`）：返回 `1`。这是自举考虑——不能让一个还没机会
  投产的新账号永远陪跑最低权重、连锚定自己所需的那次选中都赢不到。
- 这个观测字段：返回最保守的 `0`。这是反封号兜底信号——`"cold"` 标签本身就是给人看的告警。

其余情况两者数值语义一致。

### 2.5 `account_scheduling` 命名空间改名与新增投影字段

自 §8.5 命名空间统一后，投影以对象名 **`account_scheduling`** 下发（cpamp 前端读取的
canonical 名称）。旧名 **`adaptive_scheduling`** 仍以逐字节相同的值并列下发，作为过渡兼容。

优先读 `account_scheduling`。两个对象都携带 §2.1–2.4 的全部字段，外加以下字段（投影构建：
`buildAccountSchedulingView`）。

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `tier_source` | `"auto"` \| `"override"` | `subscription_tier`（§2.1）来自手动且 provider 匹配的 `tier_override`（`"override"`），还是来自 `rate_limit_tier` / `chatgpt_plan_type` 自动识别（`"auto"`）。读时从 override 是否存在派生，非单独持久化字段。空值、非法值或跨 provider 的 override 读作 `"auto"`——与 tier 解析器恰好忽略这些的行为一致。前端据此显示"手动"档位标。 |
| `rate_scale` | number, > 0 | 作用在该账号派生速率上限上的有效乘子（§3.4）。`1.0` = 不缩放。 |
| `in_distress` | bool | 账号当前是否出现养号健康门控识别的风控征兆（§3.6）。纯观测；门控关闭时为 `false`。 |
| `warmup_health_stage_cap` | number \| null | 持久化的健康允许最高养号档位索引（`0` = 第一档/最严格档）。没有记录任何降档时为 `null`（从未被减速过）——`null` 绝不能读成"被压到第 0 档"。 |
| `warmup_last_distress_at` | string, RFC3339 UTC \| null | 最近一次记录到的风控征兆墙钟时间。从未出过征兆时为 `null`。 |
| `sessions_total` / `sessions_active` / `sessions_closed` | number | 该账号已记录请求上观测到的去重 session-id 计数，按空闲时长分桶（默认 10 分钟内活跃、30 分钟后关闭）。`0` 可能表示"尚未观测到任何 session"或"未接入用量存储"——两者刻意不区分（零 session 本身就是有效答案，不像 `quota_utilization` 那样存在"快照缺失"的歧义）。 |

**与 §2.4 的关系。** 养号健康门控生效后，`warmup.stage` / `rpm_limit` / `daily_budget` /
`concurrency_limit` 反映的是**有效（被健康门控压低后）**的档位，而 `warmup.age_days` 仍是
原始自然日账龄——因此读者可以呈现"账龄 N 天但被减速到 `<stage>`"（§3.6）。

## 3. 运维说明

### 3.1 `tier_override`：手动钉死订阅档

自动识别映射不出上游值时，手动钉死一个订阅档。

有些真实生产账号上游返回的 `rate_limit_tier` 是自动识别逻辑刻意不映射的值（例如
`default_claude_ai`——一个未收录的值绝不会被"猜"成某个已知档位）。这类账号否则会一直落在
`subscription_tier = unknown`，无法参与按等级加权选号。

怎么设置：

- 通过端点（§3.5）设置，或手工编辑 auth JSON：在顶层 `"metadata"` 对象里加一个字符串字段
  `"tier_override"`。
- 合法取值：`"max_20x"` / `"max_5x"` / `"pro"`。
- 取值不区分大小写、自动去除首尾空白。空值或非法值都会被忽略，自动回退到自动识别路径——
  没有合法覆盖时现有行为完全不受影响。

为什么放在顶层、而不是 `quota_snapshot` 内部：额度轮询刷新（约 45 分钟一轮）会整体替换
`quota_snapshot` 子对象，写进它内部的值会在下一次刷新被覆盖冲掉；写在顶层则不受影响。

canonical 写入位置：端点 `PATCH /v0/management/auth-files/account-scheduling`（§3.5）是
canonical 写入方——它写到命名空间化的 `account_scheduling` 对象（§3.3），并重载运行中的
选择器。直接编辑 auth JSON 仍然有效（旧的裸顶层 key 会被 dual-read）。

典型用途：自动识别不准（如上述 `default_claude_ai` 场景），或需要人为模拟/测试某个等级的
加权选号行为时使用。

### 3.2 `first_production_at`：新鲜度锚点

这是账号新鲜度（养号年龄）的唯一锚点，决定它落在 `warmup-curve` 的哪一档、以及新鲜度加权
系数。

- **是什么**：这个账号第一次成功服务一次真实请求的墙钟时间。一次性打上时间戳、此后永不
  改写。
- **何时铸造**：由后续调用方（选号/执行路径）在账号第一次成功服务真实请求时调用
  `EnsureAuthFirstProductionAt` 铸造并持久化。失败的请求、`count_tokens` 预检请求、
  ephemeral Home dispatch 都刻意不会触发铸造——只有真正成功的一次实际服务才算数。§2.3/§2.4
  的投影本身是纯读的，绝不会主动铸造这个锚点。
- **append-only，不被额度刷新覆盖**：
  - 它刻意**不是**账号文件的 mtime 或 `CreatedAt`——两者都会被无关的 token/额度刷新写入、
    或一次 re-auth（会替换底层文件）悄悄改动，都不足以在账号整个生命周期内保持稳定。
  - 它存在同一个 `Metadata` map 里（和 `quota_snapshot` / `rate_limit_tier` 相邻），但只在
    从未设置过、或已存值损坏无法解析时才会被（重新）写入一次。已有的合法值任何后续调用都
    不会覆盖。
- **对养号档位的影响**：
  - 账号年龄 = 当前时间 − 这个锚点，取整数天数后对照 `warmup-curve` 各阶段的
    `[min-age-days, max-age-days)` 区间查表；越过最后一档进入 `mature-limits`。
  - **没有**这个锚点的账号（`"cold"` 态）会被限制在曲线**第一档（最严格档）**的限流阈值下，
    但选号权重侧的新鲜度系数按 `1` 处理（见 §2.4 分歧说明），以便这类账号仍有机会赢得一次
    选中、从而完成自己的锚定。
- **运维回填通道**：
  - 除上面 append-only 的自动铸造外，运维可以通过端点（§3.5）显式设置或清除这个锚点。
  - 用途：迁移那些在启用自适应调度**之前**就已经养号/投产的账号——否则自动铸造会把它们
    打成全新账号并压到最严格的养号档。
  - 校验：只有未来时间戳会被拒绝；任何过去日期都被接受，正确性由运维负责。
  - 安全方向：把锚点设得**早于**真实时间会让账号显得比实际更成熟（养号更少），这是有账号
    安全风险的方向，所以只回填你确认过的日期。
  - 清除后重新打开自动铸造（下一次真实成功服务会重新打一个新锚点）。

### 3.3 `account_scheduling` metadata 命名空间（dual-read / dual-emit）

所有运维/自动的调度状态都放在账号 auth JSON `metadata` 里一个**顶层** `account_scheduling`
对象下。

- 持久化子键：`tier_override`、`first_production_at`、`rate_scale`、
  `warmup_health_stage_cap`、`warmup_last_distress_at`。（`tier_source` 是投影读时派生的，
  不持久化。）
- 为什么放在一个顶层对象、而不是嵌套在 `quota_snapshot` 内部：这正是它们能在约 45 分钟一轮
  的额度刷新后存活的原因。刷新会整体替换 `quota_snapshot` 子对象，但 `Auth.Clone` 会把每个
  顶层 metadata key 原样拷贝过去。

行为：

- **dual-read（迁移）**：读时优先命名空间化子键，回退到旧的**裸顶层** key。只有
  `tier_override` 和 `first_production_at` 有 §8.5 之前的裸键形态，所以只有这两个做
  dual-read；`rate_scale`、`warmup_health_stage_cap`、`warmup_last_distress_at` 是在对象
  内部引入的，没有裸键形态。
- **写入只走新位置**：铸造/设置一律写命名空间化子键，绝不写裸键——这是非破坏性迁移（已有
  的旧值读时仍被认，但绝不被改写）。
- **清除同时删两处**：清除这些字段中的任何一个都会同时删掉命名空间化子键和旧裸键，这样
  下一次刷新时就不会有陈旧的裸值通过 dual-read 复活。
- **投影 dual-emit**：账号列表响应同时下发 `account_scheduling`（canonical）和
  `adaptive_scheduling`（旧名，值相同）——见 §2.5。

### 3.4 `rate_scale`：per-账号安全测试速率乘子

给低风险安全测试用的 per-账号速率乘子。它只改变被选中账号*能跑多快*，从不改变选择器
*选中哪个*账号。

`rate_scale` 作用在账号**派生**速率上限——rpm / burst / 并发 / 日预算——上，在 tier/养号
档位推导**之后**再乘，并且刻意**独立于选号权重**。

- **config 默认值**：`account-scheduling.rate-scale`（float，默认 **`1.0`**）。
- **per-账号覆盖**：metadata `account_scheduling.rate_scale`（dual-read 也认旧裸键
  `rate_scale`），可通过 §3.5 的端点设置。
- **解析顺序**：合法的 per-账号覆盖（存在且 `> 0`）→ config 默认值（`> 0` 时）→ `1.0`。任一
  层出现非正或无法解析的值都会跳过、用下一层，所以有效乘子始终 `> 0`，`1.0` 始终是安全的
  空操作。
- **含义**：`1.0` = 无影响；`< 1` 把每个上限压到低于其 tier/养号档位值（用于低风险安全
  测试）；`> 1` 抬高。
- **养号期同样生效**：它缩放账号当前所在的那个上限（养号阶段或成熟态上限），并不局限于
  成熟号。
- **floor 保正数、绝不缩成 0**：config 加载时会拒绝非正的 `rate-scale`；读路径上分数乘子
  会把派生的整数上限四舍五入到最近整数并 floor 到 `1`，所以小乘子能压慢一个账号，但绝不会
  把一个正的上限永久卡到 `0`。非正/无上限的值（例如成熟号 `0` = 日预算不限）保持不变。

### 3.5 管理端点：`PATCH /v0/management/auth-files/account-scheduling`

运行时设置/清除账号运维覆盖的端点——`tier_override`、`rate_scale`、`first_production_at`。
它持久化后让运行中的选择器观察到（`authManager.Update`），并返回刷新后的投影。

鉴权：admin 门控，与同类 auth-file 端点相同的 `/v0/management` admin 鉴权——
`X-Management-Key: <key>` 或 `Authorization: Bearer <key>`，无新增豁免。

请求体（`application/json`）：

| 字段 | 是否必填 | 含义 |
| --- | --- | --- |
| `name` | **必填** | auth id / 文件名 / 显示名。 |
| `auth_index` | 可选 | 消歧。 |
| `tier_override` / `rate_scale` / `first_production_at` | **至少一个存在** | 字段**是否存在**决定意图——不传 = 不改；显式空字符串或 JSON `null` = 清除；给值 = 设置。三者中至少一个必须存在，否则 `400`。 |

校验：

- `tier_override`：必须是 `max_20x` / `max_5x` / `pro` 之一，否则 `400` 并带
  `legal_values` 列表。
- `rate_scale`：数字 `> 0`，否则 `400`。
- `first_production_at`：RFC3339 **且不在未来**，否则 `400`；任何过去日期都接受。

清除是双删（命名空间化 + 旧裸键，防复活）：

- 清 `tier_override` 让 `tier_source` 回退到 `"auto"`。
- 清 `rate_scale` 回退到 config 默认值（否则 `1.0`）。
- 清 `first_production_at` 重新打开 append-only 自动铸造。

其他响应：`400` 请求体非法/无覆盖字段；`404` 账号未找到；`409` plugin-virtual auth；
`503` auth manager 不可用；`500` 持久化失败。

**设置示例** —— 钉死档位、限速到一半、回填锚点：

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

**清除示例** —— 空字符串或 `null` 清除；`first_production_at` 不传即不改：

```bash
curl -sS -X PATCH https://<host>/v0/management/auth-files/account-scheduling \
  -H "X-Management-Key: <management-key>" \
  -H "Content-Type: application/json" \
  -d '{ "name": "AC-14.json", "tier_override": "", "rate_scale": null }'
```

**成功 `200`** —— `account_scheduling` 的值就是刷新后的完整投影（§2.1–2.5）：

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

### 3.6 养号健康门控（ANCHOR-Q4）

养号升档受健康门控、不再纯按账龄：账号是**按账龄 *和* 健康**一起爬养号曲线的。有效养号档 =
`min(账龄档, 健康允许的档位上限)`；出现风控早期征兆会把上限压下来、并一直压住直到账号恢复。
仅作用于 Claude 账号，与本特性整体适用范围一致（§1）。

关键不变量：

- **只降不升**（fail-safe）：门控绝不会把账号推到高于其账龄本身应得的档位。
- **出征兆减速退档**：命中一次征兆，上限按 `demote-step` 档下降（floor 到最严格档），并打上
  `warmup_last_distress_at`。评级视图（rpm / 日预算 / 并发）和选号权重视图（新鲜度系数 /
  成熟度）都从同一个读侧 clamp 看到被压低的档位。
- **回升缓慢且受健康门控**：只有一次健康的*成功*，且当前有 cap、距上次征兆已 >=
  `promote-cooldown-minutes`，才把上限**回升一档**；每步都重新起算冷静期，所以每个冷静期
  最多回升一档。上限回到账龄应得档后就整体清掉（回到纯账龄）。回升要求健康 + 冷静期——账号
  不能只靠熬账龄在持续失败时爬升。
- **成熟号排除**（不降档）：越过整条曲线的账号按纯账龄治理；成熟号出征兆已由冷却 / 额度
  降权 / 自动隔离覆盖，刻意不把它推回养号日预算硬门控。
- **cold 号跳过**：未锚定（`"cold"`）的账号不带 cap。

配置（`account-scheduling.health-gate.*`；默认值是保守的 fail-safe 取值——design 没钉死
具体数字，待用 201 真实数据校准）：

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `enabled` | `true` | 总开关。`false` = 有效档恒等于账龄档（ANCHOR-Q4 之前的行为）。 |
| `failure-cluster-threshold` | `3` | 观察窗口内标记为 distress 的失败请求数。`0` 关闭该信号。 |
| `backoff-level-threshold` | `1` | `Quota.BackoffLevel`（不断抬升的 plan-quota 429 退避指数）达到该值即标记 distress。`1` = 任意活跃的 plan-quota 退避。`0` 关闭该信号。 |
| `observation-window-minutes` | `30` | 失败聚簇往回累计多久（仅当 `failure-cluster-threshold > 0` 时有意义）。 |
| `demote-step` | `1` | 每次命中征兆上限下降的档数（启用时必须 `>= 1`）。 |
| `promote-cooldown-minutes` | `30` | 距上次征兆多久后、一次健康成功才能把上限回升一档（启用时必须 `> 0`）。 |

两个 distress 信号是 **OR** 关系——失败聚簇或退避层级任一单独成立即标记 distress。`enabled`
时 config 加载要求两个阈值中至少一个为正。

运维读取（投影，§2.5）：`in_distress`（当前正在报征兆）、`warmup_health_stage_cap`
（持久化的 cap，`null` = 无）、`warmup_last_distress_at`（上次减速的时间）。对比
`warmup.stage`（有效、被压低后）和 `warmup.age_days`（原始账龄），就能看出一个被压到低于
账龄的账号。

信号精度（v1）：门控只用 core 已经在跟踪的两个信号——近窗口失败聚簇和 `Quota.BackoffLevel`
——**不**新增精确的 429 分类。硬失败（隔离 / 重认证 / 活跃冷却）不归这一层管；它们已经被从
可选池里过滤掉。上面的阈值都是保守默认值，待用 201 真实流量校准。

### 3.7 账号页展示（cpamp 管理前端）

cpamp 账号页直接渲染 §2.5 的投影字段；运维这样读：

- **订阅档徽标**（`20x` / `5x` / `Pro` / `未知`）：`subscription_tier`。上游返回未收录的
  `rate_limit_tier`（如 `default_claude_ai`）时显示 `未知` 是预期、不是 bug——若该账号应
  参与按档加权选号，用 `tier_override`（§3.1 / §3.5）钉死一个档位。
- **养号徽标**：`warmup.stage`（有效/被压低后的档位）配 `warmup.age_days`。
- **"手动"标**：`tier_source = "override"`。
- **会话数**：`sessions_total` / `sessions_active` / `sessions_closed`。
- **减速态**：`in_distress = true`（配合 `warmup_health_stage_cap` /
  `warmup_last_distress_at`）表示健康门控把这个账号减速了。

## 4. 注意事项

- **养号期新号受保护**：新账号（`"cold"` 或曲线内早期阶段）的日预算 / RPM / 并发都被压得
  很低（默认曲线第一档仅 200/日、3 RPM、并发 1），刻意远低于成熟号，目的是把突发流量自然
  路由到成熟账号，而不是集中打在刚上线、最脆弱的新账号上。
- **成熟号放开限制**：账号越过整条 `warmup-curve`（默认账龄 >= 60 天）后进入
  `mature-limits`：不再设固定日预算，改为按额度余量驱动；RPM / 并发 / 突发上限也放宽到一个
  刻意留有余量、只拦截病态突发流量的水位（不是日常吞吐会碰到的水位）。
- **重启后限流 / 日预算计数从内存态、非持久态重建**：per-account token bucket
  （`AccountRateLimiter`）、每日请求计数与并发在途计数（`AccountConcurrencyGate`）全部只
  存在进程内存里，**不落盘、不持久**。进程重启后：
  - token bucket 重新从"满桶"状态起步（允许一次性把配置的 burst 用满）；
  - 当日请求计数、并发在途计数都清零重新累计。

  这意味着重启前后的实际效果是**偏宽松而不是偏保守**——比如某账号当天已经打满
  `daily_budget`，进程重启会让这个计数清零，该账号当天实质上重新获得了一轮配额余量。这是
  设计上刻意接受的"安全出错方向"（宁可跨重启边界稍微多放一点，也不为这套本质上短生命周期
  的计数状态另起一套持久化子系统），不是缺陷。
  - 与此相对，**`first_production_at` 锚点是持久化的**（写在账号 auth JSON 文件的
    `metadata` 里），完全不受进程重启影响——决定养号档位的账龄判断在重启前后保持一致，
    只有限流 / 日预算 / 并发这些"计数器"状态会重建。

## 代码索引

从上文正文里移出的符号/文件位置（已对照 2026-09 当前代码核实）：

| 机制 / 字段 | 代码位置 |
| --- | --- |
| 选号加权（`AccountSelectionWeight`） | `sdk/cliproxy/auth/account_weight.go` |
| 每账号 token bucket（`AccountRateLimiter`） | `sdk/cliproxy/auth/account_rate_limiter.go` |
| 管理 API 投影写入点 | `internal/api/handlers/management/auth_files.go`（约第 490 行） |
| 旧名投影构建（`buildAdaptiveSchedulingView`） | `internal/api/handlers/management/auth_files_adaptive_scheduling.go` |
| 投影 dual-emit 下发点 | `internal/api/handlers/management/auth_files.go` |
