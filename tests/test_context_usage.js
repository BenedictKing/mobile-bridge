'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function loadRender(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function formatTokenCount'),end=source.indexOf('function compactAvailable',start);
  const node={hidden:true,textContent:'',title:''};
  const context={$:()=>node,t:text=>text,BridgeI18n:{locale:()=> 'zh-CN'}};
  vm.runInNewContext(source.slice(start,end)+'\nthis.render=renderContextUsage;',context);
  return{node,render:context.render};
}

test('missing usage is explicit without hiding the context region',()=>{
  const{node,render}=loadRender();
  render({});
  assert.equal(node.hidden,false);
  assert.equal(node.textContent,'· —');
  assert.match(node.title,/完成一轮后更新/);
});

test('settled usage keeps exact token count and percentage',()=>{
  const{node,render}=loadRender();
  render({usage:{used:13000,size:960000}});
  assert.equal(node.hidden,false);
  assert.equal(node.textContent,'· 13k/960k');
  assert.match(node.title,/1%$/);
});

test('restored usage is marked as a replay',()=>{
  const{node,render}=loadRender();
  render({usage:{used:48066,size:960000,stale:true}});
  assert.equal(node.textContent,'· ~48k/960k');
  assert.match(node.title,/上次记录的上下文用量/);
  assert.match(node.title,/tokens · 5%$/);
});
