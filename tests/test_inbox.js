'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const inbox=require('../web/inbox.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.className='';this.textContent='';this.type='';}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  find(predicate){if(predicate(this))return this;for(const child of this.children){const found=child.find?.(predicate);if(found)return found;}return null;}
  text(){return this.textContent+this.children.map(child=>child.text?child.text():'').join('');}
}
function withDom(){global.document={createElement:tag=>new FakeNode(tag)};}
const item=(overrides={})=>({agent:'kimi',sessionId:'km-1',host:'local',title:'计划会话',requestId:'0',kind:'plan',summary:'第一步',supported:true,planVersion:2,...overrides});

test('非法响应被拒绝，缺少会话身份的行被丢弃',()=>{
  assert.throws(()=>inbox.items({}),/待处理列表格式不正确/);
  assert.deepEqual(inbox.items({items:[item(),{kind:'plan'},null,item({sessionId:''})]}).map(row=>row.sessionId),['km-1']);
});

test('同一聊天的多条请求合并为一组并保持顺序',()=>{
  const groups=inbox.group([item({requestId:'a'}),item({sessionId:'cx-1',agent:'codex',requestId:'b'}),item({requestId:'c'})]);
  assert.deepEqual(groups.map(group=>[group.sessionId,group.items.map(row=>row.requestId)]),[['km-1',['a','c']],['cx-1',['b']]]);
});

test('渲染分组、类型与版本，点击打开原聊天',()=>{
  withDom();
  const container=new FakeNode('div'),opened=[];
  const count=inbox.render(container,[item(),item({requestId:'1',kind:'permission',summary:'make test',supported:false,planVersion:undefined})],(id,host)=>opened.push([id,host]));
  assert.equal(count,2);
  const text=container.text();
  assert.ok(text.includes('计划会话'));
  assert.ok(text.includes('计划审阅')&&text.includes('版本 2'));
  assert.ok(text.includes('权限确认')&&text.includes('需在桌面处理'));
  assert.ok(text.includes('2 项待处理'));
  container.find(node=>node.className==='inbox-open').onclick();
  // 卡片视觉上是一个整体：点请求摘要也应打开同一聊天。
  container.find(node=>node.className==='inbox-items').onclick();
  assert.deepEqual(opened,[['km-1','local'],['km-1','local']]);
});

test('空列表显示空状态',()=>{
  withDom();
  const container=new FakeNode('div');
  assert.equal(inbox.render(container,[],()=>{}),0);
  assert.ok(container.text().includes('没有待处理的请求'));
});

test('迟到的旧响应不覆盖新结果，失败可重试',async()=>{
  withDom();
  const container=new FakeNode('div');
  const pending=[];let counts=[];
  const controller=inbox.controller({container,request:()=>new Promise((resolve,reject)=>pending.push({resolve,reject})),open:()=>{},onCount:count=>counts.push(count)});
  const first=controller.refresh(),second=controller.refresh();
  pending[1].resolve({items:[item()]});await second;
  pending[0].resolve({items:[item(),item({requestId:'x'})]});await first;
  assert.deepEqual(counts,[1]);
  assert.ok(container.text().includes('1 项待处理'));

  const third=controller.refresh();
  pending[2].reject(new Error('网络错误'));await third;
  assert.ok(container.text().includes('网络错误'));
  const retry=container.find(node=>node.tag==='button'&&node.textContent==='重试');
  assert.ok(retry);
  retry.onclick();
  pending[3].resolve({items:[]});await new Promise(resolve=>setImmediate(resolve));
  assert.ok(container.text().includes('没有待处理的请求'));
});

test('未传 onCount 时仍渲染结果（app.js 的接法）',async()=>{
  withDom();
  const container=new FakeNode('div');
  const controller=inbox.controller({container,request:async()=>({items:[item()]}),open:()=>{}});
  await controller.refresh();
  assert.ok(container.text().includes('计划会话'));
});

test('点面板外的遮罩关闭弹窗，点面板内边距或内容不关闭',()=>{
  const listeners={};let closed=0;
  const dialog={addEventListener:(type,fn)=>{listeners[type]=fn;},close:()=>{closed++;},getBoundingClientRect:()=>({left:10,top:300,right:380,bottom:800})};
  inbox.closeOnBackdrop(dialog);
  listeners.click({target:{},clientX:5,clientY:100});
  listeners.click({target:dialog,clientX:20,clientY:310});
  assert.equal(closed,0);
  listeners.click({target:dialog,clientX:200,clientY:100});
  assert.equal(closed,1);
});
