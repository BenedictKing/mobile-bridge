#!/bin/zsh
# 隧道看门狗：Python 时代 cloudflare-tunnel-watchdog（/ready 探测 + 唤醒半死
# 检测）的 launchd 等价物。launchd 的 KeepAlive 只重启已退出的进程；睡眠唤
# 醒后 cloudflared 可能活着但不干活，这里连续 LIMIT 次探测不健康即强制重启。
#
# 探测两道：cloudflared 本地 metrics /ready，以及公网端到端回路（固定域名
# /api/auth 返回 2xx）。域名从 <repo>/.local/config.json 的 connections 自动
# 发现。端到端回路只在本地网关在运行时才参与判定——网关被人为停止时，不把
# 「源站关闭」误判成隧道故障。
#
# 用法：tunnel-watchdog.sh <repo> [launchd-label]
# 环境覆盖：TUNNEL_METRICS_PORT（默认 49769）、GATEWAY_PORT（默认 8787）、
# TUNNEL_WATCHDOG_LIMIT（默认 3）。
set -u
REPO="${1:?usage: tunnel-watchdog.sh <repo> [launchd-label]}"
LABEL="${2:-com.mobile-bridge.cloudflared}"
METRICS_PORT="${TUNNEL_METRICS_PORT:-49769}"
GATEWAY_PORT="${GATEWAY_PORT:-8787}"
LIMIT="${TUNNEL_WATCHDOG_LIMIT:-3}"
STATE="$REPO/.local/tunnel-watchdog.state"
LOG="$REPO/.local/tunnel.log"

public_url() {
  /usr/bin/env python3 - "$REPO/.local/config.json" <<'PY'
import json, sys
try:
    for entry in json.load(open(sys.argv[1])).get('connections', []):
        if entry.get('enabled') and entry.get('accessMode') == 'cloudflare' and entry.get('publicUrl'):
            print(entry['publicUrl'].rstrip('/'))
            break
except Exception:
    pass
PY
}

note() { echo "$(date '+%Y-%m-%dT%H:%M:%S') tunnel watchdog: $*" >> "$LOG"; }

healthy=1
curl -fsS -m 4 "http://127.0.0.1:${METRICS_PORT}/ready" >/dev/null 2>&1 || healthy=0
if [ "$healthy" = 1 ]; then
  gateway_up=0
  curl -fsS -m 3 -o /dev/null "http://127.0.0.1:${GATEWAY_PORT}/api/health" && gateway_up=1
  if [ "$gateway_up" = 1 ]; then
    url="$(public_url)"
    if [ -n "$url" ] && ! curl -fsS -m 15 -o /dev/null "$url/api/auth"; then
      healthy=0
    fi
  fi
fi

if [ "$healthy" = 1 ]; then
  print -r -- 0 >| "$STATE"
  exit 0
fi

count="$(cat "$STATE" 2>/dev/null)"
[[ "$count" == <-> ]] || count=0
count=$(( count + 1 ))
print -r -- "$count" >| "$STATE"
note "探测不健康（$count/$LIMIT）"
if [ "$count" -ge "$LIMIT" ]; then
  print -r -- 0 >| "$STATE"
  if launchctl kickstart -k "gui/$(id -u)/$LABEL"; then
    note "已强制重启 $LABEL"
  else
    note "重启 $LABEL 失败（代理未加载？）"
  fi
fi
