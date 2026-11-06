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

test('turn diff summary keeps five files expanded', () => {
  const card = loadDiff().summaryCard(changes(5), '', () => {});
  assert.equal(card.tagName, 'DIV');
  assert.equal(card.children[0].tagName, 'DIV');
  assert.equal(card.children[1].children.length, 5);
});

test('turn diff summary collapses more than five files by default', () => {
  let opened = null;
  const card = loadDiff().summaryCard(changes(6), '', change => { opened = change; });
  assert.equal(card.tagName, 'DETAILS');
  assert.equal(card.open, false);
  assert.equal(card.children[0].tagName, 'SUMMARY');
  const count = card.children[0].children.find(node => node.tagName === 'STRONG');
  assert.equal(count.textContent, '6');
  assert.equal(card.children[1].children.length, 6);
  card.open = true;
  card.children[1].children[5].onclick();
  assert.equal(opened.path, 'web/file-6.js');
});

test('demo turn diff summary uses the same collapse threshold', () => {
  const source = readFileSync(require.resolve('../site/demo-product/web/diff.js'), 'utf8');
  const production = readFileSync(require.resolve('../web/diff.js'), 'utf8');
  const start = source.indexOf('  function summaryCard(');
  const end = source.indexOf('\n  let dialog', start);
  assert.ok(start >= 0 && end > start);
  assert.ok(production.includes(source.slice(start, end)));
});
