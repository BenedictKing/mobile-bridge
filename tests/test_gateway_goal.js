'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {WorkModes}=require('../web/modes.js');

class Node {
  constructor(tag='div') {this.tag=tag;this.children=[];this.hidden=false;this.disabled=false;this.value='';this.textContent='';this.open=false;}
  append(...children){this.children.push(...children);}
  replaceChildren(...children){this.children=[...children];}
  get firstElementChild(){return this.children[0];}
  querySelector(){return this.option||null;}
  setAttribute(){}
  focus(){}
}

function setup(provider='gateway'){
  const values=new Map(),calls=[];
  const storage={getItem:key=>values.get(key)||null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};
  const select=new Node('select');select.option=new Node('option');
  const root=new Node(),toggle=new Node('button'),hint=new Node();
  global.document={createElement:tag=>new Node(tag)};
  global.BridgeI18n={t:value=>value,locale:()=>"zh-CN"};
  const modes=new WorkModes({select,hint,goalRoot:root,goalToggle:toggle,storage,newId:()=>"op-1",
    onStatus:async payload=>calls.push(payload),onCancel:async()=>{},onEdit:async()=>{},onContinue:async()=>{},onComplete:async()=>{},onReview:async payload=>calls.push(payload)});
  modes.open('chat');
  modes.render({host:'local',status:'idle',capabilities:['goals'],goalRuntimeAvailable:true,
    goal:{provider,goalId:'goal-1',objective:'整理仓库',status:'waiting_review',revision:7,autoStepsUsed:1,autoStepsLimit:5}},'send',false);
  return {modes,root,calls};
}

test('gateway Goal actions carry state and revision for CAS',async()=>{
  const {modes,calls}=setup();
  await modes.setStatus('paused');
  assert.equal(calls.length,1);
  assert.equal(calls[0].id,'op-1');
  assert.equal(calls[0].expectedState,'waiting_review');
  assert.equal(calls[0].revision,7);
});

test('native Goal actions keep their existing request shape',async()=>{
  const {modes,calls}=setup('native');
  await modes.setStatus('paused');
  assert.equal(Object.hasOwn(calls[0],'expectedState'),false);
  assert.equal(Object.hasOwn(calls[0],'revision'),false);
});

test('waiting-review Goal offers manual decision controls',()=>{
  const {root}=setup();
  const actions=root.children.find(node=>node.className==='goal-actions');
  assert.ok(actions);
  assert.ok(actions.children.some(node=>node.textContent==='人工继续一轮'));
  assert.ok(actions.children.some(node=>node.textContent==='标记完成'));
});

test('unknown delivery requires an explicit verified review action',async()=>{
  const {modes,root,calls}=setup();
  modes.render({...modes.view,goal:{...modes.view.goal,status:'outcome_unknown'}},'send',false);
  const actions=root.children.find(node=>node.className==='goal-actions');
  const review=actions.children.find(node=>node.textContent==='已核实，进入复核');
  assert.ok(review);
  await review.onclick();
  assert.equal(calls[0].verified,true);
  assert.equal(calls[0].expectedState,'outcome_unknown');
  assert.equal(calls[0].revision,7);
});

test('compact mode menu keeps per-chat selection and shows only the active mode',()=>{
  const saved=new Map(),options={};
  for(const value of ['default','plan','goal'])options[value]=Object.assign(new Node('option'),{hidden:false,disabled:false});
  const select=new Node('select');
  select.querySelector=selector=>options[/\[value="?([^\]"]+)/.exec(selector)?.[1]]||null;
  const classList=()=>({toggle(){}});
  const button=Object.assign(new Node('button'),{classList:classList(),contains:()=>false});
  const label=new Node(),items=['default','plan','goal'].map(value=>Object.assign(new Node('button'),{dataset:{workMode:value},classList:classList()}));
  const menu=Object.assign(new Node('div'),{querySelectorAll:()=>items,contains:()=>false});
  global.document={createElement:tag=>new Node(tag),addEventListener(){}};
  const modes=new WorkModes({select,button,label,menu,hint:new Node(),goalRoot:new Node(),goalToggle:new Node(),
    storage:{getItem:key=>saved.get(key)||null,setItem:(key,value)=>saved.set(key,value),removeItem:key=>saved.delete(key)},
    onCancel:async()=>{},onStatus:async()=>{},onEdit:async()=>{},onContinue:async()=>{},onComplete:async()=>{},onReview:async()=>{}});
  modes.open('compact');modes.render({host:'local',status:'idle',capabilities:['goals'],goalRuntimeAvailable:true},'send',false);
  assert.equal(label.textContent,'模式');
  button.onclick();assert.equal(menu.hidden,false);
  items[1].onclick();
  assert.equal(saved.get('work-mode:compact'),'plan');
  assert.equal(label.textContent,'计划');
  assert.equal(menu.hidden,true);
});
