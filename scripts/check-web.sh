#!/usr/bin/env bash
# 前端语法检查：把 HTML 里的内联 <script> 抽出来交给 node --check。
#
# 为什么要这一步：前端是零构建的（没有 bundler 帮忙做语法检查），
# 一个手滑的括号只会在浏览器里静默失败。CI 里必须挡住。
set -euo pipefail

cd "$(dirname "$0")/.."

command -v node >/dev/null 2>&1 || { echo "需要 node 来做语法检查" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0

node --check web/app.js && echo "  ok: web/app.js" || fail=1

for html in web/*.html; do
  base="$(basename "$html")"
  out="$TMP/${base}.js"
  python3 - "$html" "$out" <<'PY'
import re
import sys

src, dst = sys.argv[1], sys.argv[2]
html = open(src, encoding="utf-8").read()
blocks = re.findall(r"<script>(.*?)</script>", html, re.S)
open(dst, "w", encoding="utf-8").write("\n;\n".join(blocks))
print(f"  {src}: 提取 {len(blocks)} 段内联脚本")
PY
  if node --check "$out"; then
    echo "  ok: $html"
  else
    echo "  语法错误: $html" >&2
    fail=1
  fi
done

exit "$fail"
