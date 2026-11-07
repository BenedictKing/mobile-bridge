'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const vm = require('node:vm');

class FakeNode {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.className = '';
    this.textContent = '';
    if (tag === 'details') this.open = false;
  }
  append(...nodes) { this.children.push(...nodes); }
}

function loadDiff() {
  const context = {
    window: {},
    document: {createElement: tag => new FakeNode(tag)},
  };
  vm.runInNewContext(readFileSync(require.resolve('../web/diff.js'), 'utf8'), context);
  return context.window.BridgeDiff;
}

function changes(count) {
  return Array.from({length: count}, (_, index) => ({
    path: `web/file-${index + 1}.js`,
    diff: '@@ -1 +1 @@\n-old\n+new',
  }));
}

test('turn diff summary starts collapsed and shows aggregate line counts', () => {
  const card = loadDiff().summaryCard(changes(5), '', () => {});
  assert.equal(card.tagName, 'DETAILS');
  assert.equal(card.open, false);
  assert.equal(card.children[0].tagName, 'SUMMARY');
  assert.equal(card.children[0].children.find(node => node.className === 'diff-stat-add').textContent, '+5');
  assert.equal(card.children[0].children.find(node => node.className === 'diff-stat-del').textContent, '−5');
  assert.equal(card.children[1].children.length, 5);
});

test('turn diff summary keeps files clickable after expansion', () => {
  let opened = null;
  const card = loadDiff().summaryCard(changes(6), '', change => { opened = change; });
  assert.equal(card.tagName, 'DETAILS');
  assert.equal(card.open, false);
  assert.equal(card.children[0].tagName, 'SUMMARY');
  assert.equal(card.children[0].children[0], '6');
  assert.equal(card.children[1].children.length, 6);
  card.open = true;
  card.children[1].children[5].onclick();
  assert.equal(opened.path, 'web/file-6.js');
});

test('truncated file counts stay explicitly incomplete', () => {
  const rows = changes(1);
  rows[0].truncated = true;
  const card = loadDiff().summaryCard(rows, '', () => {});
  assert(card.children[0].children.includes('部分统计不可用'));
  assert(!card.children[1].children[0].children.some(node => node.className === 'diff-stat-add'));
});

test('demo turn diff summary uses the same collapse threshold', () => {
  const source = readFileSync(require.resolve('../site/demo-product/web/diff.js'), 'utf8');
  const production = readFileSync(require.resolve('../web/diff.js'), 'utf8');
  const start = source.indexOf('  function summaryCard(');
  const end = source.indexOf('\n  let dialog', start);
  assert.ok(start >= 0 && end > start);
  assert.ok(production.includes(source.slice(start, end)));
});
