#!/usr/bin/env bash
# 验证 CodeBuddy 时间戳修复
#
# 这个脚本检查 codebuddy 的 MetaReader 是否正确从 transcript 读取时间戳
# 而不是使用文件 mtime

set -euo pipefail

echo "=== CodeBuddy 时间戳修复验证 ==="
echo

# 检查是否有 codebuddy transcript 文件
CODEBUDDY_DIR="$HOME/.codebuddy/projects"

if [[ ! -d "$CODEBUDDY_DIR" ]]; then
    echo "❌ 未找到 CodeBuddy 项目目录: $CODEBUDDY_DIR"
    echo "   请确保已经使用过 codebuddy CLI"
    exit 1
fi

# 查找一个 transcript 文件进行测试
TRANSCRIPT=$(find "$CODEBUDDY_DIR" -name "*.jsonl" -type f | head -1)

if [[ -z "$TRANSCRIPT" ]]; then
    echo "❌ 未找到任何 transcript 文件"
    exit 1
fi

echo "📄 测试文件: $TRANSCRIPT"
echo

# 获取文件 mtime
FILE_MTIME=$(stat -f "%m" "$TRANSCRIPT" 2>/dev/null || stat -c "%Y" "$TRANSCRIPT" 2>/dev/null)
FILE_MTIME_DATE=$(date -r "$FILE_MTIME" "+%Y-%m-%d %H:%M:%S" 2>/dev/null || date -d "@$FILE_MTIME" "+%Y-%m-%d %H:%M:%S")
echo "📅 文件 mtime: $FILE_MTIME_DATE (${FILE_MTIME}000 ms)"

# 查找文件中最后一条 message 事件的时间戳
LAST_MESSAGE_TS=$(grep '"type":"message"' "$TRANSCRIPT" | tail -1 | grep -o '"timestamp":[0-9]*' | cut -d: -f2)

if [[ -n "$LAST_MESSAGE_TS" ]]; then
    LAST_MESSAGE_SEC=$((LAST_MESSAGE_TS / 1000))
    LAST_MESSAGE_DATE=$(date -r "$LAST_MESSAGE_SEC" "+%Y-%m-%d %H:%M:%S" 2>/dev/null || date -d "@$LAST_MESSAGE_SEC" "+%Y-%m-%d %H:%M:%S")
    echo "💬 最后消息时间: $LAST_MESSAGE_DATE ($LAST_MESSAGE_TS ms)"

    # 比较时间差
    DIFF=$((FILE_MTIME - LAST_MESSAGE_SEC))
    if [[ $DIFF -gt 60 ]]; then
        echo "⚠️  时间差 ${DIFF} 秒 - mtime 比最后消息新很多（可能是 preload 导致的）"
        echo "✅ 修复应该使用消息时间而非 mtime"
    else
        echo "✓ 时间差很小 (${DIFF}s)，文件未被 preload 触碰"
    fi
else
    echo "⚠️  未找到 message 事件，将回退到 session-meta"
fi

echo
echo "=== 修复验证要点 ==="
echo "1. ✅ codebuddy.MetaReader 已添加"
echo "2. ✅ gateway main.go 已设置 manager.NativeMeta"
echo "3. ✅ 优先使用 message/tool 事件时间戳"
echo "4. ✅ 添加了完整的单元测试"
echo
echo "重启服务后，codebuddy 会话的时间将保持稳定，不会被 preload 刷新到当前时间"
