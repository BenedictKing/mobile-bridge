"""Read-only helper for Mobile / Bridge on a remote Codex host.

The gateway ships this file over stdin to ``python3 -`` on a computer the
desktop App already reaches through a saved SSH alias, then appends a few
lines that decode one JSON request and print one JSON reply. Nothing is
installed or written here: the helper only runs the read-only query the
gateway built and reads rollout files inside this host's Codex data directory.
"""
import base64
import json
import os
import sqlite3
from pathlib import Path
from urllib.parse import quote

ROLLOUT_CAP = 8 * 1024 * 1024


class Fail(Exception):
    """A refusal the gateway shows the user as-is."""


def codex_home():
    return Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")


def connect():
    candidates = [path for path in codex_home().glob("state_*.sqlite") if path.is_file()]
    if not candidates:
        raise Fail("远端找不到 Codex 会话数据库，请在该电脑打开 Codex 后重试")
    newest = max(candidates, key=lambda path: path.stat().st_mtime)
    try:
        db = sqlite3.connect("file:" + quote(str(newest)) + "?mode=ro", uri=True, timeout=8)
        db.execute("PRAGMA query_only = ON")
    except sqlite3.Error as exc:
        raise Fail("远端暂时无法读取 Codex 会话数据库，请稍后重试") from exc
    return db


def json_value(value):
    if isinstance(value, bytes):
        return value.decode("utf-8", "replace")
    return value


def schema():
    db = connect()
    try:
        columns = [row[1] for row in db.execute("PRAGMA table_info(threads)")]
    except sqlite3.Error as exc:
        raise Fail("远端暂时无法读取 Codex 会话数据库，请稍后重试") from exc
    finally:
        db.close()
    if "id" not in columns:
        raise Fail("远端找不到 Codex 会话数据库，请在该电脑打开 Codex 后重试")
    return {"columns": columns}


def query(request):
    sql = request.get("sql") or ""
    if not sql.lstrip().upper().startswith("SELECT"):
        raise Fail("远端只允许只读查询")
    db = connect()
    try:
        rows = db.execute(sql, request.get("args") or []).fetchall()
    except sqlite3.Error as exc:
        raise Fail("远端暂时无法读取 Codex 会话数据库，请稍后重试") from exc
    finally:
        db.close()
    return {"rows": [[json_value(value) for value in row] for row in rows]}


def rollout(thread_id):
    if not thread_id:
        raise Fail("找不到这个会话")
    db = connect()
    try:
        cursor = db.execute("SELECT * FROM threads WHERE id = ?", (thread_id,))
        names = [column[0] for column in cursor.description]
        rows = cursor.fetchall()
    except sqlite3.Error as exc:
        raise Fail("远端暂时无法读取 Codex 会话数据库，请稍后重试") from exc
    finally:
        db.close()
    if len(rows) != 1:
        raise Fail("找不到这个会话")
    row = {name: json_value(value) for name, value in zip(names, rows[0])}
    stored = row.get("rollout_path") or ""
    if not stored:
        raise Fail("找不到这个会话的历史记录")
    home = codex_home()
    roots = [os.path.realpath(str(home / name)) for name in ("sessions", "archived_sessions")]
    path = os.path.realpath(stored)
    if not any(path == root or path.startswith(root + os.sep) for root in roots):
        raise Fail("会话记录路径不在 Codex 数据目录中")
    if not os.path.isfile(path):
        raise Fail("找不到这个会话的历史记录")
    size = os.path.getsize(path)
    with open(path, "rb") as stream:
        if size > ROLLOUT_CAP:
            # Keep the newest turns; the partial first line is dropped and the
            # gateway reports that older turns are not loaded.
            stream.seek(size - ROLLOUT_CAP)
            data = stream.read(ROLLOUT_CAP).partition(b"\n")[2]
        else:
            data = stream.read()
    return {"row": row, "content": data.decode("utf-8", "replace"), "truncated": size > ROLLOUT_CAP}


def handle(request):
    op = request.get("op")
    if op == "schema":
        return schema()
    if op == "query":
        return query(request)
    if op == "rollout":
        return rollout(request.get("thread_id") or "")
    raise Fail("未知的远端读取操作")
