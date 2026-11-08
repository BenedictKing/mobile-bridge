'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const {ChatTimeline}=require('../web/timeline.js');

class FakeClassList{
  constructor(node){this.node=node;}
  toggle(name,on){const names=new Set(this.node.className.split(/\s+/).filter(Boolean));if(on)names.add(name);else names.delete(name);this.node.className=[...names].join(' ');}
  contains(name){return this.node.className.split(/\s+/).includes(name);}
}
class FakeNode{
  constructor(tag='div'){
    this.tagName=tag.toUpperCase();this.children=[];this.dataset={};this.className='';this.classList=new FakeClassList(this);
    this.attributes={};this.hidden=false;this.textContent='';this.scrollTop=0;this.scrollHeight=2000;this.clientHeight=600;
    this.rect={top:100,bottom:700};this.listeners=new Map();
  }
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=nodes;}
  querySelector(){return null;}
  querySelectorAll(){return [];}
  addEventListener(type,handler){this.listeners.set(type,handler);}
  removeEventListener(type){this.listeners.delete(type);}
  setAttribute(name,value){this.attributes[name]=String(value);}
  removeAttribute(name){delete this.attributes[name];}
  getBoundingClientRect(){return this.rect;}
}

function setup(){
  const ids={};
  for(const id of ['timeline','messages','older','newer','user-jump','user-jump-list','user-jump-count'])ids[id]=new FakeNode(id==='user-jump'?'details':'div');
  ids['user-jump'].hidden=true;
  global.document={documentElement:new FakeNode(),getElementById:id=>ids[id],createElement:tag=>new FakeNode(tag)};
  global.ResizeObserver=class{observe(){}disconnect(){}};
  global.requestAnimationFrame=callback=>{callback();return 1;};
  const timeline=new ChatTimeline({url:action=>action,request:async()=>({}),renderMeta(){},renderText(){},status(){}});
  return{timeline,ids};
}

function addUser(timeline,ids,key,text,top){
  const row={key,role:'user',text,order:timeline.rows.size};
  const node=new FakeNode();node.dataset.key=key;node.rect={top,bottom:top+80};
  timeline.rows.set(key,{row,sequence:1});timeline.nodes.set(key,{node,stamp:''});ids.messages.children.push(node);
  return row;
}

test('user jump lists only user messages and marks the latest at bottom',()=>{
  const{timeline,ids}=setup();
  const first=addUser(timeline,ids,'u1','第一条\n需要换行的消息',120);
  const second=addUser(timeline,ids,'u2','第二条消息',500);
  timeline.renderUserJump([first,{key:'a1',role:'assistant',text:'回复',order:1.5},second],true);
  assert.equal(ids['user-jump'].hidden,false);
  assert.equal(ids['user-jump-count'].textContent,'2');
  assert.equal(ids['user-jump-list'].children.length,2);
  assert.equal(ids['user-jump-list'].children[0].children[1].textContent,'第一条 需要换行的消息');
  assert.equal(ids['user-jump-list'].children[1].attributes['aria-current'],'location');
  assert.equal(ids['user-jump-list'].scrollTop,ids['user-jump-list'].scrollHeight);
  timeline.dispose();
});

test('user jump click scrolls to the message and scroll tracking updates the active item',()=>{
  const{timeline,ids}=setup();
  const first=addUser(timeline,ids,'u1','第一条',120),second=addUser(timeline,ids,'u2','第二条',500);
  timeline.renderUserJump([first,second]);
  ids['user-jump-list'].children[1].onclick();
  assert.equal(ids.timeline.scrollTop,388);
  assert.equal(timeline.following,false);
  assert.equal(ids['user-jump-list'].children[1].classList.contains('active'),true);
  timeline.updateUserJumpActive();
  assert.equal(ids['user-jump-list'].children[0].attributes['aria-current'],'location');
  assert.equal(ids['user-jump-list'].children[1].attributes['aria-current'],undefined);
  timeline.renderUserJump([]);
  assert.equal(ids['user-jump'].hidden,true);
  timeline.dispose();
});

test('user jump uses a compact flow entry on phones and a floating panel on large screens',()=>{
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.user-jump\{display:block;flex:none;align-self:flex-end/);
  assert.match(css,/\.user-jump-list\{max-height:min\(30dvh,280px\)/);
  assert.match(css,/@media\(min-width:721px\)\{\s*\.user-jump\{position:absolute/);
  assert.doesNotMatch(css,/\.user-jump\{display:none\}/);
});

test('user jump lists prompts from the meta index beyond the loaded window',()=>{
  const{timeline,ids}=setup();
  timeline.userIndex=[{key:'u-old',text:'很早的一条消息'},{key:'u1',text:'最新消息'}];
  const row=addUser(timeline,ids,'u1','最新消息',120);
  timeline.renderUserJump([row]);
  assert.equal(ids['user-jump'].hidden,false);
  assert.equal(ids['user-jump-count'].textContent,'2');
  assert.equal(ids['user-jump-list'].children.length,2);
  assert.equal(ids['user-jump-list'].children[0].children[1].textContent,'很早的一条消息');
  timeline.dispose();
});

test('jumping to an unloaded prompt pages history until the node exists',async()=>{
  const{timeline,ids}=setup();
  const newest=addUser(timeline,ids,'u1','最新消息',120);
  timeline.epoch='e1';timeline.sequence=1;timeline.hasMore=true;timeline.before=null;
  timeline.userIndex=[{key:'u-old',text:'很早的一条消息'},newest];
  timeline.renderUserJump([newest]);
  timeline.read=async()=>({epoch:'e1',sequence:2,before:'b0',hasMore:false,meta:{},
    rows:[{key:'u-old',role:'user',text:'很早的一条消息',order:0}]});
  const originalRender=timeline.renderRow.bind(timeline);
  timeline.renderRow=(row,open)=>{const node=originalRender(row,open);if(row.key==='u-old')node.rect={top:300,bottom:380};return node;};
  await timeline.jumpToUser('u-old');
  assert.ok(timeline.nodes.has('u-old'),'paged row not loaded');
  assert.equal(ids.timeline.scrollTop,188,'viewport did not scroll to the row');
  assert.equal(timeline.userJumpButtons.get('u-old').classList.contains('active'),true);
  timeline.dispose();
});

test('ACP latest user edits use a branch while Codex keeps in-place edits',()=>{
  for(const testCase of [
    {agent:'kimi',label:'编辑并新建分支',action:'edit-fork'},
    {agent:'zcode',label:'编辑并新建分支',action:'edit-fork'},
    {agent:'codex',label:'编辑并重新发送',action:'edit'},
  ]){
    const{timeline}=setup();
    const node=new FakeNode(),bar=new FakeNode();
    node.querySelector=selector=>selector==='.message-actions'?bar:null;
    timeline.nodes.set('u1',{node,stamp:''});
    timeline.rows.set('u1',{row:{key:'u1',turnId:'turn-1',editable:true,turnStatus:'completed'},sequence:1});
    timeline.meta={agent:testCase.agent,latestUserTurnId:'turn-1',status:'idle'};
    let action='';timeline.onAction=value=>{action=value;};
    timeline.refreshActions();
    assert.equal(bar.children.length,1);
    assert.equal(bar.children[0].textContent,testCase.label);
    bar.children[0].onclick();
    assert.equal(action,testCase.action);
    timeline.dispose();
  }
});

test('scrolling away from the latest message shows a persistent jump-to-latest button',()=>{
  const{timeline,ids}=setup();
  assert.equal(ids.newer.hidden,true);
  assert.equal(ids.newer.textContent,'跳转到最后 ↓');
  ids.timeline.scrollTop=0;
  timeline.onScroll();
  assert.equal(timeline.following,false);
  assert.equal(ids.newer.hidden,false);
  assert.equal(ids.newer.textContent,'跳转到最后 ↓');
  ids.newer.textContent='有新内容 ↓';
  ids.timeline.scrollTop=20;
  timeline.onScroll();
  assert.equal(ids.newer.textContent,'有新内容 ↓','scrolling must not erase the new-content hint');
  ids.newer.onclick();
  assert.equal(ids.timeline.scrollTop,ids.timeline.scrollHeight);
  assert.equal(timeline.following,true);
  assert.equal(ids.newer.hidden,true);
  assert.equal(ids.newer.textContent,'跳转到最后 ↓');
  timeline.dispose();
});

test('steered event is a readable status without raw JSON',()=>{
  const{timeline}=setup();
  const node=timeline.renderRow({role:'activity',kind:'steered',title:'steered',text:'{"type":"steered","id":"internal"}'},false);
  assert.equal(node.tagName,'DIV');
  assert.equal(node.children.length,1);
  assert.equal(node.children[0].textContent,'已向进行中的对话补充指示');
  timeline.dispose();
});

test('command rows are collapsed to an accessible status and expand to full text',()=>{
  const cases=[
    [{kind:'commandExecution',status:'inProgress',text:'go test ./...'},'执行命令（进行中）','inProgress'],
    [{kind:'commandExecution',status:'completed',exitCode:0,text:'go test ./...\nPASS'},'执行命令（成功）','completed'],
    [{kind:'commandExecution',status:'completed',exitCode:2,text:'a very long command\nmultiple\noutput lines'},'执行命令（失败）','failed'],
    [{kind:'CommandExecution',status:'cancelled',text:'stop this'},'执行命令（已取消）','cancelled'],
    [{kind:'commandExecution',status:'completed',text:'missing exit code'},'执行命令（结果未知）','unknown'],
    [{kind:'commandExecution',status:'outcome_unknown',exitCode:0,text:'uncertain'},'执行命令（结果未知）','unknown'],
    [{kind:'toolCall',toolKind:'execute',status:'failed',text:'make check'},'执行命令（失败）','failed'],
  ];
  for(const [fixture,label,state] of cases){
    const{timeline}=setup();
    const row={key:'command-1',role:'activity',version:1,truncated:true,...fixture};
    const node=timeline.renderRow(row,false);
    assert.equal(node.tagName,'DETAILS');
    assert.equal(node.open,false,'new commands must start collapsed');
    assert.equal(node.dataset.tool,'');
    assert.equal(node.dataset.command,'');
    assert.equal(node.dataset.commandStatus,state);
    assert.equal(node.children[0].tagName,'SUMMARY');
    assert.equal(node.children[0].textContent,label);
    assert.ok(!node.children[0].textContent.includes(row.text),'summary must not expose command or output');
    assert.equal(node.children[1].textContent,row.text,'expanded body keeps command and output');
    assert.equal(node.children[1].hidden,false);
    assert.equal(node.children[2].hidden,false,'truncated rows keep continuation control');
    const opened=timeline.renderRow(row,true);
    assert.equal(opened.open,true,'redraw preserves an open command');
    timeline.dispose();
  }
});

test('command CSS hides all body preview and keeps non-command activities unchanged',()=>{
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.activity\[data-command\]:not\(\[open\]\) \.activity-body/);
  assert.match(css,/\.activity summary:focus-visible/);
  const{timeline}=setup();
  const thought=timeline.renderRow({key:'thought',role:'activity',kind:'reasoning',title:'思考摘要',text:'摘要'},false);
  assert.equal(thought.dataset.command,undefined);
  assert.equal(thought.dataset.tool,undefined);
  timeline.dispose();
});
