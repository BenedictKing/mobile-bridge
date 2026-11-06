'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function loadDeleteHelpers(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function deleteChatConfirmation'),end=source.indexOf('let deleteChatBusy',start);
  const context={t:value=>value};
  vm.runInNewContext(source.slice(start,end),context);
  return context;
}

test('Codex confirmation discloses the hidden-record fallback',()=>{
  const text=loadDeleteHelpers().deleteChatConfirmation(true);
  assert.match(text,/尝试/);
  assert.match(text,/原生删除失败/);
  assert.match(text,/记录可能保留/);
});

test('delete result distinguishes native deletion and each hidden mode',()=>{
  const {deleteChatMessage}=loadDeleteHelpers();
  assert.equal(deleteChatMessage({mode:'deleted'},true),'会话已删除');
  assert.equal(deleteChatMessage({mode:'hidden',warning:'unsupported'},true),'原生删除失败，已从列表移除；电脑端记录可能仍保留');
  assert.equal(deleteChatMessage({mode:'hidden'},true),'已从列表移除；电脑端记录可能仍保留');
  assert.equal(deleteChatMessage({mode:'hidden'},false),'已从列表移除；Agent 侧记录仍保留');
});
