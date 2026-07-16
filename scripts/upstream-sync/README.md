# core 上游定期自动同步

每周把上游 `router-for-me/CLIProxyAPI` 的改动**准备**成可审阅状态,交人/AI 决策后合入。
本 fork 有大量本地反关联/指纹改动,与上游持续在同一批文件上分叉,**"整体无冲突自动合"极少发生**,
所以这套机制的定位是"自动准备 + 清单驱动决策",**不做无人值守自动合**。

## 组成

| 文件 | 作用 |
|---|---|
| `analyze-upstream-sync.sh` | 只读分析,生成三份清单 + 机器可读判定(可本地跑) |
| `sensitive-paths.txt` | 反关联/指纹敏感路径(git pathspec),用于标"无冲突语义风险" |
| `../../.github/workflows/upstream-sync.yml` | 每周 cron + 手动触发的编排 workflow |

## 三份清单

1. **上游变更清单**:上次同步基线(merge-base) → 上游最新之间所有 commit(按类型分类、版本区间、高 churn 目录)。
2. **直接冲突清单**:直接 merge 会撞的文件(只读三方合并探测,分组:源码/测试/机械)。
3. **无冲突语义风险清单**:上游和 fork **都改过同一文件、git 自动合了、但语义可能打架**的文件
   ——"能编译但悄悄改了行为",落在反关联敏感区的置顶。这是无冲突合并的主要风险来源。

## workflow 行为

- **上游无新 commit** → 跳过。
- **无冲突** → 在 `bot/upstream-sync` 分支完成 merge、跑 `go test`,开/更新 PR(打 `upstream-sync` label),
  正文含三份清单 + go test 结果。**不 auto-merge**——即使全绿也停在等人工 approve。
- **有冲突** → 开/更新一个 `upstream-sync` label 的 Issue(含三份清单),交人工按手工流程解冲突,不 push 半成品 PR。

## 前提:PAT secret

workflow 必须用 PAT,不能用默认 `GITHUB_TOKEN`(否则自动开的 PR 不触发本仓 CI,GitHub 防递归限制)。
在 **core 仓**(`0xTract0r/CLIProxyAPIPlus`)配置 secret `UPSTREAM_SYNC_TOKEN`,权限:
本仓 `contents:write` + `pull_requests:write` + `issues:write`。

## 手动触发 / 本地验证

- 手动触发(GitHub Actions → Upstream Sync → Run workflow),勾选 `dry_run` 只生成清单摘要、不产生 PR/Issue。
- 本地跑清单(只读,需先 `git fetch upstream --tags && git fetch origin`):

  ```bash
  bash scripts/upstream-sync/analyze-upstream-sync.sh \
    --fork-ref origin/main --upstream-ref upstream/main --output /tmp/sync-report.md
  ```

## 维护

- 上游若重构导致反关联相关路径迁移,同步更新 `sensitive-paths.txt`(与 fork 反关联工作同源)。
- GitHub 对 60 天无提交的仓库会自动禁用 scheduled workflow;core 通常活跃,若长期沉寂需手动重启该 workflow 或补 keepalive。
- 解冲突后开 PR 必须带 `upstream-sync` label,否则 `translator-path-guard` 会挡住(合入 `internal/translator/**`)。
