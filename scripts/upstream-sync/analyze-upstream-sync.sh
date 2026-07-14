#!/usr/bin/env bash
#
# analyze-upstream-sync.sh — core fork 上游同步分析(只读)
#
# 对照"上次同步基线(merge-base) → 上游最新",生成三份可审阅清单 + 机器可读判定:
#   ① 上游变更清单     :基线到上游 tip 之间所有 commit(按类型分类、版本区间、高 churn 目录)
#   ② 直接冲突清单     :直接 merge 会撞的文件(只读三方合并探测,分组:机械/源码/测试)
#   ③ 无冲突语义风险清单:上游改了、能自动合、但落在 fork 反关联/指纹敏感区的文件(需 AI/人工核语义)
#
# 只读保证:仅用 rev-list/diff/merge-base/merge-tree(--write-tree 只写 object db、不动 refs/index/worktree)。
# 不做 fetch、不切分支、不写工作树。调用方需先 git fetch upstream/origin。
#
# 用法:
#   bash analyze-upstream-sync.sh [--fork-ref REF] [--upstream-ref REF] \
#        [--sensitive-file PATH] [--output PATH]
# 默认: --fork-ref origin/main  --upstream-ref upstream/main
# 机器可读判定:若环境变量 GITHUB_OUTPUT 已设置,追加 has_conflict/conflict_count/... 供 workflow 消费。

set -euo pipefail

FORK_REF="origin/main"
UPSTREAM_REF="upstream/main"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SENSITIVE_FILE="${SCRIPT_DIR}/sensitive-paths.txt"
OUTPUT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --fork-ref) FORK_REF="$2"; shift 2 ;;
    --upstream-ref) UPSTREAM_REF="$2"; shift 2 ;;
    --sensitive-file) SENSITIVE_FILE="$2"; shift 2 ;;
    --output) OUTPUT="$2"; shift 2 ;;
    -h|--help)
      grep -E '^#' "$0" | sed -E 's/^# ?//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

# --- 前置校验 ---------------------------------------------------------------
git rev-parse --git-dir >/dev/null 2>&1 || { echo "不在 git 仓库内" >&2; exit 3; }
for ref in "$FORK_REF" "$UPSTREAM_REF"; do
  git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null \
    || { echo "无法解析 ref: ${ref}(是否已 git fetch upstream/origin?)" >&2; exit 3; }
done
[[ -f "$SENSITIVE_FILE" ]] || { echo "找不到敏感路径清单: ${SENSITIVE_FILE}" >&2; exit 3; }

TMPDIR_WORK="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_WORK"' EXIT

# --- 基础拓扑 ---------------------------------------------------------------
MB="$(git merge-base "$FORK_REF" "$UPSTREAM_REF")"
UP_TIP="$(git rev-parse "$UPSTREAM_REF")"
FORK_TIP="$(git rev-parse "$FORK_REF")"
mb_desc="$(git describe --tags "$MB" 2>/dev/null || git rev-parse --short "$MB")"
up_desc="$(git describe --tags "$UP_TIP" 2>/dev/null || git rev-parse --short "$UP_TIP")"

upstream_ahead="$(git rev-list --count "${MB}..${UPSTREAM_REF}")"
fork_ahead="$(git rev-list --count "${MB}..${FORK_REF}")"

# --- ① 上游改动文件全集 ------------------------------------------------------
upstream_changed="${TMPDIR_WORK}/upstream_changed.txt"
git diff --name-only "$MB" "$UPSTREAM_REF" | sort -u > "$upstream_changed"
upstream_changed_count="$(wc -l < "$upstream_changed" | tr -d ' ')"

# 上游 commit 按 conventional type 分类(非常规归 other)
type_table="${TMPDIR_WORK}/types.txt"
# awk 跨平台一致(BSD/macOS sed 的 t 空 label 语法不可移植)
git log --format='%s' "${MB}..${UPSTREAM_REF}" \
  | awk '{
      if (match($0, /^[a-zA-Z]+(\([^)]*\))?!?:/)) {
        t=$0; sub(/[(!:].*/, "", t); print tolower(t)
      } else { print "other" }
    }' \
  | sort | uniq -c | sort -rn > "$type_table"

# --- ② 直接冲突探测(只读 merge-tree) ----------------------------------------
mt_raw="${TMPDIR_WORK}/merge_tree.raw"
set +e
git merge-tree --write-tree --name-only "$FORK_REF" "$UPSTREAM_REF" > "$mt_raw" 2>/dev/null
mt_exit=$?
set -e
conflict_file="${TMPDIR_WORK}/conflicts.txt"
if [[ $mt_exit -eq 0 ]]; then
  : > "$conflict_file"                          # 干净:0 冲突
elif [[ $mt_exit -eq 1 ]]; then
  # 首行是 tree oid;其后到第一个空行为止是冲突文件名
  awk 'NR==1{next} /^$/{exit} {print}' "$mt_raw" | sort -u > "$conflict_file"
else
  # 其它退出码(如 128)是 merge-tree 自身错误,不能当成"0 冲突"放行
  echo "git merge-tree 异常退出(${mt_exit});无法判定冲突,请检查 refs 是否正确。" >&2
  exit 4
fi
conflict_count="$(wc -l < "$conflict_file" | tr -d ' ')"
if [[ "$conflict_count" -eq 0 ]]; then has_conflict=false; else has_conflict=true; fi

# 冲突分组
conf_tests="$(grep -E '_test\.go$' "$conflict_file" || true)"
conf_src="$(grep -E '\.go$' "$conflict_file" | grep -vE '_test\.go$' || true)"
conf_other="$(grep -vE '\.go$' "$conflict_file" || true)"
conf_src_count="$(printf '%s\n' "$conf_src" | grep -c . || true)"
conf_tests_count="$(printf '%s\n' "$conf_tests" | grep -c . || true)"
conf_other_count="$(printf '%s\n' "$conf_other" | grep -c . || true)"

# --- ③ 无冲突语义风险 = 两边都改过、自动合(无文本冲突)的文件 ----------------
# 定义:上游和 fork 都改过同一文件、git 能无冲突自动合,但两边逻辑可能语义打架
# ——"能编译但悄悄改了行为"。这是无冲突合并的主要风险来源,比"敏感目录里上游单方
# 改动"精准得多(fork 没碰的文件没有自己的逻辑要被破坏)。

# fork 侧改动全集
fork_changed="${TMPDIR_WORK}/fork_changed.txt"
git diff --name-only "$MB" "$FORK_REF" | sort -u > "$fork_changed"

# 敏感命中(上游改动 ∩ 敏感 pathspec),用于给风险文件标"是否落在命脉区"
specs=()
while IFS= read -r line; do
  [[ -n "$line" ]] && specs+=("$line")
done < <(grep -vE '^[[:space:]]*(#|$)' "$SENSITIVE_FILE")
sensitive_changed="${TMPDIR_WORK}/sensitive_changed.txt"
if [[ ${#specs[@]} -gt 0 ]]; then
  git diff --name-only "$MB" "$UPSTREAM_REF" -- "${specs[@]}" | sort -u > "$sensitive_changed"
else
  : > "$sensitive_changed"
fi

# 两边都改 → 减去冲突 = 两边改且自动合(无冲突语义风险主体)
both_changed="${TMPDIR_WORK}/both_changed.txt"
comm -12 "$fork_changed" "$upstream_changed" > "$both_changed"
amb="${TMPDIR_WORK}/amb.txt"
comm -23 "$both_changed" "$conflict_file" > "$amb"
# 拆分:落在敏感命脉区(最高优先) / 其余
amb_sensitive="${TMPDIR_WORK}/amb_sensitive.txt"
amb_other="${TMPDIR_WORK}/amb_other.txt"
comm -12 "$amb" "$sensitive_changed" > "$amb_sensitive"
comm -23 "$amb" "$sensitive_changed" > "$amb_other"
# 敏感区冲突(既冲突、又在命脉区)
sens_conflict_file="${TMPDIR_WORK}/sens_conflict.txt"
comm -12 "$sensitive_changed" "$conflict_file" > "$sens_conflict_file"
# 仅上游单方改的敏感文件(fork 未碰,语义风险较低,只给计数不逐条列)
upstream_only_sensitive="${TMPDIR_WORK}/uos.txt"
comm -23 "$sensitive_changed" "$fork_changed" > "$upstream_only_sensitive"

amb_count="$(wc -l < "$amb" | tr -d ' ')"
amb_sensitive_count="$(wc -l < "$amb_sensitive" | tr -d ' ')"
amb_other_count="$(wc -l < "$amb_other" | tr -d ' ')"
sens_conflict_count="$(wc -l < "$sens_conflict_file" | tr -d ' ')"
uos_count="$(wc -l < "$upstream_only_sensitive" | tr -d ' ')"
sensitive_risk_count="$amb_count"

# --- 结论 -------------------------------------------------------------------
if [[ "$has_conflict" == "true" ]]; then
  verdict="⚠️ 有 ${conflict_count} 个文本冲突,需人工/AI 解冲突后才能合入"
else
  verdict="✅ 0 文本冲突,可在同步分支完成 merge 并跑 go test,全绿后等人工 approve"
fi

# --- 渲染 Markdown ----------------------------------------------------------
render() {
  echo "# core 上游同步分析"
  echo
  echo "- 上游最新: \`${up_desc}\` (\`${UP_TIP:0:12}\`)"
  echo "- 上次同步基线: \`${mb_desc}\` (\`${MB:0:12}\`)"
  echo "- 当前 fork: \`${FORK_TIP:0:12}\` (\`${FORK_REF}\`)"
  echo
  echo "## 摘要"
  echo
  echo "| 指标 | 值 |"
  echo "|---|---|"
  echo "| 上游领先 commit | ${upstream_ahead} |"
  echo "| fork 本地领先 commit(分叉规模) | ${fork_ahead} |"
  echo "| 上游改动文件 | ${upstream_changed_count} |"
  echo "| 直接合并冲突文件 | ${conflict_count} |"
  echo "| └─ 其中落在敏感区(最高优先) | ${sens_conflict_count} |"
  echo "| 两边都改、自动合(无冲突语义风险) | ${amb_count} |"
  echo "| └─ 其中在 fork 敏感区(最高优先核语义) | ${amb_sensitive_count} |"
  echo "| 结论 | ${verdict} |"
  echo
  echo "## ① 上游变更清单(\`${mb_desc}\` → \`${up_desc}\`, ${upstream_ahead} commit)"
  echo
  echo "按 commit 类型:"
  echo
  echo '```'
  cat "$type_table"
  echo '```'
  echo
  echo "高 churn 目录(按改动文件数,前 20):"
  echo
  echo '```'
  # 包一层 || true:改动目录极多时 head 提前关闭管道会给 git SIGPIPE,pipefail+set -e 会误中断
  { git diff --dirstat=files,0 "$MB" "$UPSTREAM_REF" || true; } | head -20
  echo '```'
  echo
  echo "## ② 直接冲突清单(${conflict_count} 个文件)"
  echo
  if [[ "$conflict_count" -eq 0 ]]; then
    echo "无文本冲突 🎉 —— 直接 merge 可干净合入。"
  else
    if [[ -n "$conf_src" ]]; then
      echo "**源码 .go(${conf_src_count} 个):**"
      echo
      printf '%s\n' "$conf_src" | sed 's/^/- /'
      echo
    fi
    if [[ -n "$conf_tests" ]]; then
      echo "**测试 _test.go(${conf_tests_count} 个):**"
      echo
      printf '%s\n' "$conf_tests" | sed 's/^/- /'
      echo
    fi
    if [[ -n "$conf_other" ]]; then
      echo "**机械/配置(README/go.mod/Dockerfile 等, ${conf_other_count} 个):**"
      echo
      printf '%s\n' "$conf_other" | sed 's/^/- /'
      echo
    fi
  fi
  echo
  echo "## ③ 无冲突语义风险清单(${amb_count} 个文件两边都改、自动合)"
  echo
  echo "> 定义:上游与 fork 都改过同一文件、git 无冲突自动合,但两边逻辑可能语义打架"
  echo "> ——\"能编译但悄悄改了行为\"。这是无冲突合并的主要风险来源。"
  echo
  if [[ "$amb_count" -eq 0 ]]; then
    echo "无(本轮没有两边同改且自动合的文件)。"
  else
    if [[ "$amb_sensitive_count" -gt 0 ]]; then
      echo "### 🔴 落在 fork 反关联/指纹敏感区(${amb_sensitive_count} 个,最高优先核语义)"
      echo
      sed 's/^/- /' "$amb_sensitive"
      echo
    fi
    if [[ "$amb_other_count" -gt 0 ]]; then
      echo "### 其余两边同改(${amb_other_count} 个)"
      echo
      sed 's/^/- /' "$amb_other"
      echo
    fi
  fi
  if [[ "$uos_count" -gt 0 ]]; then
    echo "> 另有 ${uos_count} 个敏感区文件仅上游单方改动、fork 未碰,语义风险较低,此处不逐条列。"
    echo
  fi
  if [[ "$sens_conflict_count" -gt 0 ]]; then
    echo "### ⚠️ 敏感区冲突(最高优先:既冲突、又在命脉区, ${sens_conflict_count} 个)"
    echo
    sed 's/^/- /' "$sens_conflict_file"
    echo
  fi
  echo "## AI 审查待办"
  echo
  echo "请对 ③ 与\"敏感区冲突\"文件逐条核查语义风险:上游改动是否改变 fork 的"
  echo "反关联/设备指纹/uTLS 传输/codex 认证/image-strip 行为。仅编译通过不足以放行,"
  echo "0 冲突也必须过 \`go test\` + AI 语义审查 + 人工 approve 才可合入。"
}

if [[ -n "$OUTPUT" ]]; then
  render > "$OUTPUT"
  echo "已写入报告: ${OUTPUT}" >&2
else
  render
fi

# --- 机器可读判定(供 workflow) ---------------------------------------------
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "has_conflict=${has_conflict}"
    echo "conflict_count=${conflict_count}"
    echo "sensitive_risk_count=${sensitive_risk_count}"
    echo "sensitive_conflict_count=${sens_conflict_count}"
    echo "upstream_ahead=${upstream_ahead}"
    echo "upstream_tip_desc=${up_desc}"
    echo "merge_base_desc=${mb_desc}"
  } >> "$GITHUB_OUTPUT"
fi

# 退出码语义:0=分析完成(无论有无冲突);非 0 仅代表脚本自身错误(前面已 exit)。
exit 0
