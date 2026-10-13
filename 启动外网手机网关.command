#!/bin/zsh
cd -- "${0:A:h}" || exit 1
# 固定域名隧道常驻于 launchd 代理 com.mobile-bridge.cloudflared（KeepAlive，
# 见 ~/Library/LaunchAgents/com.mobile-bridge.cloudflared.plist，token 经钥匙串
# 注入后写入该 0600 plist 的环境变量）。代理未安装时退回手动拉起。
if ! pgrep -f 'cloudflared tunnel .*cloudflare-mobile-bridge\.yml' >/dev/null 2>&1; then
  if ! launchctl kickstart -k "gui/$(id -u)/com.mobile-bridge.cloudflared" 2>/dev/null; then
    CLOUDFLARED="$(command -v cloudflared || print -- /opt/homebrew/bin/cloudflared)"
    [[ -x "$CLOUDFLARED" ]] || { echo "未找到 cloudflared：$CLOUDFLARED"; exit 1; }
    "$CLOUDFLARED" tunnel --config "${PWD}/.local/cloudflare-mobile-bridge.yml" \
      --no-autoupdate --protocol http2 --metrics 127.0.0.1:49769 \
      >>"${PWD}/.local/tunnel.log" 2>&1 &!
  fi
fi
[[ -x dist/gateway/codex-mobile-gateway ]] || make build-gateway-package || exit 1
exec "${PWD}/dist/gateway/codex-mobile-gateway" serve --data-dir "${PWD}/.local"
