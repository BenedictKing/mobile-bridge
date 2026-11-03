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

test('user jump is large-screen only',()=>{
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.user-jump\{display:none\}/);
  assert.match(css,/@media\(min-width:721px\)\{\s*\.user-jump\{display:block/);
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
