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
    onStatus:async payload=>calls.push(payload),onCancel:async()=>{},onEdit:async()=>{},onContinue:async()=>{},onComplete:async()=>{}});
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
