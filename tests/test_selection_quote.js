'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const BridgeSelection=require('../web/selection.js');

test('单行选区生成一段引用',()=>{
  assert.equal(BridgeSelection.quoteBlock('把这段改成异步'),'> 把这段改成异步');
});

test('多行选区逐行加引用前缀，空行只留 >',()=>{
  assert.equal(BridgeSelection.quoteBlock('第一行\n\n第三行'),'> 第一行\n>\n> 第三行');
});

test('CRLF 与前导空行先归一化再引用',()=>{
  assert.equal(BridgeSelection.quoteBlock('\r\n收尾两行\r\n第二行  \r\n'),'> 收尾两行\n> 第二行');
});

test('首行缩进与制表符保留：引用不吞掉原文缩进',()=>{
  assert.equal(BridgeSelection.quoteBlock('  indented\n\ttabbed'),'>   indented\n> \ttabbed');
});

test('未超长的选区不截断',()=>{
  assert.deepEqual(BridgeSelection.clampText('  short  '),{text:'  short',truncated:false});
});

test('超长选区在换行处截断并标记省略',()=>{
  const line='甲'.repeat(100);
  const result=BridgeSelection.clampText(Array.from({length:60},()=>line).join('\n'));
  assert.equal(result.truncated,true);
  const lines=result.text.split('\n');
  assert.equal(lines.length,40,'39 整行 + 省略行');
  assert.equal(lines[39],'…');
  assert.ok(lines.slice(0,39).every(item=>item.length===100));
});

test('单行超长选区按长度截断',()=>{
  const result=BridgeSelection.clampText('乙'.repeat(BridgeSelection.MAX_CHARS+50));
  assert.equal(result.truncated,true);
  assert.equal(result.text,'乙'.repeat(BridgeSelection.MAX_CHARS)+'\n…');
});

test('空输入框：引用后留一个空行给意见',()=>{
  assert.equal(BridgeSelection.appendQuote('',BridgeSelection.quoteBlock('原文')),'> 原文\n\n');
});

test('已有草稿：引用接在末尾，用户的话仍在前面',()=>{
  assert.equal(BridgeSelection.appendQuote('帮我改一下\n\n',BridgeSelection.quoteBlock('原文')),'帮我改一下\n\n> 原文\n\n');
});

test('已有草稿末尾空白先清掉，不叠出多余空行',()=>{
  assert.equal(BridgeSelection.appendQuote('  草稿   ',BridgeSelection.quoteBlock('原文')),'  草稿\n\n> 原文\n\n');
});
