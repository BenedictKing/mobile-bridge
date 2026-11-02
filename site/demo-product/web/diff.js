'use strict';
// 每轮文件结果卡片与差异审阅面板。数据来自 timeline 行上的结构化字段:
// row.changes = [{path, diff}] (codex, unified 文本) 或
// [{path, oldText, newText}] (ACP, 自行按行 LCS);row.exitCode 给命令行
// 加 ✓/✗ 徽标。所有内容一律 textContent 构建,聊天内容永远不进 HTML。
window.BridgeDiff = (() => {
  const LCS_CAP = 2000;   // 单侧超过此行数退化为「仅显示新文本」
  const BATCH = 400;      // 面板首批渲染行数,之后分批继续显示
  const t = s => typeof BridgeI18n === 'undefined' ? s : BridgeI18n.t(s);
  const splitLines = text => String(text ?? '').split('\n');

  // 相对 cwd 的显示路径;cwd 未知时原样返回。
  function displayPath(path, cwd) {
    if (!path) return '';
    if (cwd && path.startsWith(cwd + '/')) return path.slice(cwd.length + 1);
    return path;
  }

  // unified diff 文本 → [{kind:'add'|'del'|'context'|'hunk', text, oldNo, newNo}]
  function unifiedRows(diff) {
    const rows = [];
    let oldNo = 0, newNo = 0;
    for (const line of splitLines(diff)) {
      if (line.startsWith('@@')) {
        const match = /@@ -(\d+)/.exec(line), plus = /\+(\d+)/.exec(line);
        oldNo = match ? parseInt(match[1], 10) : oldNo;
        newNo = plus ? parseInt(plus[1], 10) : newNo;
        rows.push({kind: 'hunk', text: line});
      } else if (line.startsWith('+++') || line.startsWith('---')) {
        continue;
      } else if (line.startsWith('+')) {
        rows.push({kind: 'add', text: line.slice(1), newNo: newNo++});
      } else if (line.startsWith('-')) {
        rows.push({kind: 'del', text: line.slice(1), oldNo: oldNo++});
      } else {
        rows.push({kind: 'context', text: line.startsWith(' ') ? line.slice(1) : line,
          oldNo: oldNo++, newNo: newNo++});
      }
    }
    return rows;
  }

  // 按行 LCS(前后缀裁剪 + O(n·m) DP,单侧超限退化为「仅显示新文本」)。
  function pairRows(oldText, newText) {
    const a = splitLines(oldText), b = splitLines(newText);
    let start = 0;
    while (start < a.length && start < b.length && a[start] === b[start]) start++;
    let endA = a.length, endB = b.length;
    while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) { endA--; endB--; }
    const head = a.slice(0, start).map((text, i) => ({kind: 'context', text, oldNo: i + 1, newNo: i + 1}));
    const tail = a.slice(endA).map((text, i) => ({kind: 'context', text, oldNo: endA + i + 1, newNo: endB + i + 1}));
    const midA = a.slice(start, endA), midB = b.slice(start, endB);
    if (midA.length > LCS_CAP || midB.length > LCS_CAP) {
      // 超限:老内容全部视为保留,新内容整体作为新增,不做精确对齐。
      const rows = head.slice();
      for (const [i, text] of midA.entries()) rows.push({kind: 'context', text, oldNo: start + i + 1});
      for (const [j, text] of midB.entries()) rows.push({kind: 'add', text, newNo: start + j + 1});
      rows.push(...tail);
      return {rows, degraded: true};
    }
    const width = midB.length + 1;
    const table = new Uint16Array(width * (midA.length + 1));
    for (let i = midA.length - 1; i >= 0; i--) {
      for (let j = midB.length - 1; j >= 0; j--) {
        table[i * width + j] = midA[i] === midB[j]
          ? table[(i + 1) * width + j + 1] + 1
          : Math.max(table[(i + 1) * width + j], table[i * width + j + 1]);
      }
    }
    const rows = head.slice();
    let i = 0, j = 0;
    while (i < midA.length && j < midB.length) {
      if (midA[i] === midB[j]) {
        rows.push({kind: 'context', text: midA[i], oldNo: start + i + 1, newNo: start + j + 1});
        i++; j++;
      } else if (table[(i + 1) * width + j] >= table[i * width + j + 1]) {
        rows.push({kind: 'del', text: midA[i], oldNo: start + i + 1});
        i++;
      } else {
        rows.push({kind: 'add', text: midB[j], newNo: start + j + 1});
        j++;
      }
    }
    for (; i < midA.length; i++) rows.push({kind: 'del', text: midA[i], oldNo: start + i + 1});
    for (; j < midB.length; j++) rows.push({kind: 'add', text: midB[j], newNo: start + j + 1});
    rows.push(...tail);
    return {rows, degraded: false};
  }

  function changeRows(change) {
    if (typeof change.diff === 'string') return {rows: unifiedRows(change.diff), degraded: false};
    return pairRows(change.oldText ?? '', change.newText ?? '');
  }

  function lineStats(change) {
    const {rows, degraded} = changeRows(change);
    let add = 0, del = 0;
    for (const row of rows) { if (row.kind === 'add') add++; else if (row.kind === 'del') del++; }
    return {add, del, degraded, total: rows.length};
  }

  function cardButton(change, cwd, onOpen) {
    const card = document.createElement('button');
    card.type = 'button';
    card.className = 'diff-card';
    const name = document.createElement('span');
    name.className = 'diff-card-path';
    name.textContent = displayPath(change.path, cwd) || change.path;
    card.append(name);
    if (change.truncated) {
      const mark = document.createElement('span');
      mark.className = 'diff-stat-mut';
      mark.textContent = t('已截断');
      card.append(mark);
    }
    if (typeof change.diff === 'string' || change.oldText != null || change.newText != null) {
      const stats = lineStats(change);
      const add = document.createElement('span'); add.className = 'diff-stat-add'; add.textContent = '+' + stats.add;
      const del = document.createElement('span'); del.className = 'diff-stat-del'; del.textContent = '−' + stats.del;
      card.append(add, del);
    } else {
      const note = document.createElement('span');
      note.className = 'diff-stat-mut';
      note.textContent = t('差异过大，仅显示文件名');
      card.append(note);
    }
    card.onclick = () => onOpen(change);
    return card;
  }

  // 行级:activity 行内的文件卡片列表。
  function fileCards(changes, cwd, onOpen) {
    const wrap = document.createElement('div');
    wrap.className = 'diff-cards';
    for (const change of changes) wrap.append(cardButton(change, cwd, onOpen));
    return wrap;
  }

  // 每轮汇总:「本轮改动 N 个文件」+ 去重后的文件卡片。
  function summaryCard(changes, cwd, onOpen) {
    const wrap = document.createElement('div');
    wrap.className = 'turn-diff-summary';
    const title = document.createElement('div');
    title.className = 'turn-diff-title';
    const count = document.createElement('strong');
    count.textContent = String(changes.length);
    title.append(t('本轮改动'), ' ', count, ' ', t('个文件'));
    wrap.append(title, fileCards(changes, cwd, onOpen));
    return wrap;
  }

  let dialog = null, body = null, titleNode = null;

  function reviewDialog() {
    if (dialog) return dialog;
    dialog = document.createElement('dialog');
    dialog.id = 'diff-dialog';
    dialog.className = 'picker';
    const head = document.createElement('div');
    head.className = 'picker-head';
    titleNode = document.createElement('h2');
    titleNode.className = 'diff-review-title';
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'icon-button';
    close.textContent = '×';
    close.setAttribute('aria-label', t('关闭'));
    close.onclick = () => dialog.close();
    head.append(titleNode, close);
    body = document.createElement('div');
    body.className = 'picker-body diff-review-body';
    dialog.append(head, body);
    document.body.append(dialog);
    return dialog;
  }

  function diffLine(row) {
    const line = document.createElement('div');
    line.className = 'diff-line ' + row.kind;
    const oldNo = document.createElement('span'), newNo = document.createElement('span');
    oldNo.className = newNo.className = 'diff-no';
    oldNo.textContent = row.oldNo ?? '';
    newNo.textContent = row.newNo ?? '';
    const code = document.createElement('code');
    code.textContent = row.text;
    if (row.kind !== 'hunk') line.append(oldNo, newNo);
    line.append(code);
    return line;
  }

  // 审阅面板:单文件差异,分批渲染。
  function open(change, cwd) {
    reviewDialog();
    body.replaceChildren();
    titleNode.replaceChildren();
    const path = displayPath(change.path, cwd) || change.path;
    const name = document.createElement('span');
    name.textContent = path;
    titleNode.append(name);
    const head = document.createElement('div');
    head.className = 'diff-file-head';
    const pathLine = document.createElement('span');
    pathLine.className = 'diff-file-path';
    pathLine.textContent = path;
    head.append(pathLine);
    const hasText = typeof change.diff === 'string' || change.oldText != null || change.newText != null;
    if (change.truncated) {
      const mark = document.createElement('span');
      mark.className = 'diff-stat-mut';
      mark.textContent = t('已截断');
      head.append(mark);
    }
    body.append(head);
    if (!hasText) {
      const note = document.createElement('p');
      note.className = 'diff-note';
      note.textContent = t('差异过大，仅显示文件名');
      body.append(note);
      dialog.showModal();
      return;
    }
    const {rows, degraded} = changeRows(change);
    if (degraded) {
      const note = document.createElement('p');
      note.className = 'diff-note';
      note.textContent = t('仅显示新增内容');
      body.append(note);
    }
    if (!rows.length) {
      const note = document.createElement('p');
      note.className = 'diff-note';
      note.textContent = t('暂无差异内容');
      body.append(note);
      dialog.showModal();
      return;
    }
    const view = document.createElement('div');
    view.className = 'diff-view';
    body.append(view);
    let shown = 0;
    const more = document.createElement('button');
    more.type = 'button';
    more.className = 'plain diff-more';
    more.textContent = t('继续显示');
    const appendBatch = () => {
      const fragment = document.createDocumentFragment();
      const end = Math.min(shown + BATCH, rows.length);
      for (; shown < end; shown++) fragment.append(diffLine(rows[shown]));
      view.append(fragment);
      more.hidden = shown >= rows.length;
    };
    appendBatch();
    more.onclick = appendBatch;
    body.append(more);
    dialog.showModal();
  }

  return {fileCards, summaryCard, open, displayPath, lineStats};
})();
