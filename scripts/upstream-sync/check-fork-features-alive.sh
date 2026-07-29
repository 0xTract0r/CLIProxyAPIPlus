#!/usr/bin/env bash
#
# check-fork-features-alive.sh — core fork 特性存活审计(只读)
#
# 读取 fork-feature-manifest.tsv,逐条断言 fork 反关联/自定义能力的符号、
# 关键调用点、自定义路由和专属文件在当前 core 代码里仍然存在。任一条目"消失"
# 视为一次上游同步/重构悄悄删掉或回退了 fork 特性,脚本非零退出。
#
# 定位:这是"go test 覆盖不到的整条接线被删"的兜底 + CI 门禁,不替代 go test,
# 也不比对完整函数签名——只做粗粒度存在性检查。
#
# 用法:
#   bash check-fork-features-alive.sh [core_root] [--manifest PATH]
# 默认:core_root 自动定位为本脚本所在目录的上上级(core/scripts/upstream-sync/../.. = core/)
#      manifest 默认为本脚本同目录下的 fork-feature-manifest.tsv
#
# 退出码:0 = 全部条目存活;1 = 至少一条 MISSING;2 = 用法/前置错误。

set -uo pipefail
# 注意:不用 set -e——脚本要在某一行 grep 未命中(退出码 1)时继续跑完剩余条目
# 并汇总结果,而不是被 grep 的非零退出提前打断。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CORE_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MANIFEST="${SCRIPT_DIR}/fork-feature-manifest.tsv"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest) MANIFEST="$2"; shift 2 ;;
    --core-root) CORE_ROOT="$2"; shift 2 ;;
    -h|--help)
      grep -E '^#' "$0" | sed -E 's/^# ?//'; exit 0 ;;
    -*)
      echo "未知参数: $1" >&2; exit 2 ;;
    *)
      CORE_ROOT="$1"; shift ;;
  esac
done

[[ -f "$MANIFEST" ]] || { echo "找不到 manifest: ${MANIFEST}" >&2; exit 2; }
[[ -d "$CORE_ROOT" ]] || { echo "core 根目录不存在: ${CORE_ROOT}" >&2; exit 2; }

total=0
passed=0
missing_lines=()

while IFS=$'\t' read -r category id method pattern relfile; do
  # 跳过注释与空行(注释行第一个字段以 # 开头)
  [[ -z "$category" ]] && continue
  [[ "$category" =~ ^# ]] && continue

  total=$((total + 1))
  target="${CORE_ROOT}/${relfile}"

  case "$method" in
    symbol|pattern|route)
      if [[ ! -e "$target" ]]; then
        missing_lines+=("[MISSING] ${category} / ${id} (方式=${method}) — 文件本身不存在: ${relfile}")
        continue
      fi
      if grep -Eq -- "$pattern" "$target" 2>/dev/null; then
        passed=$((passed + 1))
      else
        missing_lines+=("[MISSING] ${category} / ${id} (方式=${method}) — 未命中 pattern=\`${pattern}\` in ${relfile}")
      fi
      ;;
    pattern_absent)
      if [[ ! -e "$target" ]]; then
        missing_lines+=("[MISSING] ${category} / ${id} (方式=${method}) — 文件本身不存在(无法判定回退与否): ${relfile}")
        continue
      fi
      if grep -Eq -- "$pattern" "$target" 2>/dev/null; then
        missing_lines+=("[MISSING] ${category} / ${id} (方式=${method}) — 检测到应已移除的回退 pattern=\`${pattern}\` 命中于 ${relfile}(反关联保护被回退)")
      else
        passed=$((passed + 1))
      fi
      ;;
    file)
      if [[ -e "$target" ]]; then
        passed=$((passed + 1))
      else
        missing_lines+=("[MISSING] ${category} / ${id} (方式=${method}) — 路径不存在: ${relfile}")
      fi
      ;;
    *)
      missing_lines+=("[MISSING] ${category} / ${id} — 未知断言方式: ${method}(manifest 格式错误)")
      ;;
  esac
done < <(grep -vE '^[[:space:]]*(#|$)' "$MANIFEST")

if [[ "$total" -eq 0 ]]; then
  echo "manifest 里没有可读的数据行: ${MANIFEST}" >&2
  exit 2
fi

if [[ ${#missing_lines[@]} -eq 0 ]]; then
  echo "PASS ${passed}/${total}"
  exit 0
else
  for line in "${missing_lines[@]}"; do
    echo "$line" >&2
  done
  echo "PASS ${passed}/${total}(${#missing_lines[@]} 条 MISSING,详见上面 stderr 明细)" >&2
  exit 1
fi
