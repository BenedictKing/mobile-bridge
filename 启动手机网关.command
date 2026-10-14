#!/bin/zsh
cd -- "${0:A:h}" || exit 1
# ACP agent 自动发现依赖 PATH 中的 npx/node（zcode/claude/dsh 的 npx 回退）；
# 固定注入 fnm 的稳定安装路径，避免无 shell 钩子的环境下丢 agent。
for _nodebin in "$HOME/.local/share/fnm/node-versions/"*/installation/bin; do
  [[ -d "$_nodebin" ]] && export PATH="$_nodebin:$PATH"
done
[[ -x dist/gateway/codex-mobile-gateway ]] || make build-gateway-package || exit 1
exec "${PWD}/dist/gateway/codex-mobile-gateway" serve --data-dir "${PWD}/.local"
