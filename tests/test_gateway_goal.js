'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
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
  return {modes,root,calls,select};
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

test('running replies explain why Goal cannot be selected',()=>{
  const {modes,select}=setup();
  modes.render({host:'local',status:'active',capabilities:['goals'],goalRuntimeAvailable:true},'steer',false);
  assert.equal(select.option.disabled,true);
  assert.equal(select.option.title,'当前回复结束后可开启目标模式');
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

function goalCommandSuggestions(view){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('const COMPACT_PREF_LABELS='),end=source.indexOf('// parseCompactCommand',start);
  const context={window:{},currentTeam:null,state:view,t:value=>value};
  vm.runInNewContext(source.slice(start,end),context);
  return context.composerCommandRows('goal');
}

test('/goal suggestion follows gateway capability for remote sessions',()=>{
  const view={host:'ssh',capabilities:['goals'],goalRuntimeAvailable:true};
  assert.ok(goalCommandSuggestions(view).some(row=>row.token==='/goal '));
  assert.equal(goalCommandSuggestions({...view,goalRuntimeAvailable:false}).some(row=>row.token==='/goal '),false);
  assert.equal(goalCommandSuggestions({...view,capabilities:[]}).some(row=>row.token==='/goal '),false);
});

function goalComposer({active=false,host='local'}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const goalStart=source.indexOf('function parseGoalCommand'),goalEnd=source.indexOf('function compactInstructions',goalStart);
  const submitStart=source.indexOf("$('composer').onsubmit="),submitEnd=source.indexOf("$('message').onkeydown",submitStart);
  const nodes={composer:new Node('form'),message:new Node('textarea'),'work-mode':new Node('select'),'send-mode':new Node('select'),send:new Node('button'),'send-error':new Node()};
  nodes['work-mode'].option=Object.assign(new Node('option'),{disabled:active,hidden:false});nodes['send-mode'].value='send';
  const values=new Map(),calls=[];
  const context={
    $:id=>nodes[id],t:value=>value,BridgeI18n:{locale:()=> 'zh-CN'},window:{BridgePresentation:{resizeMessage(){}}},
    sending:false,currentId:'chat',currentHost:host,currentTeam:null,state:{host,status:active?'active':'idle',capabilities:['goals'],goalRuntimeAvailable:true},
    closeSkillSuggestions(){},parseCompactCommand:()=>null,parseMigrateCommand:()=>null,attachments:{ready:()=>true,ids:()=>[],setLocked(){},clear(){}},
    invalidSkills:new Set(),selectedSkills:new Set(),sessionStorage:{getItem:key=>values.get(key)||null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)},
    workModes:{selection:null,value:()=>nodes['work-mode'].value,render(){},submitted(){}},
    chatKey:()=> host+'|chat',sessionUrl:()=> '/api/sessions/chat/send?host='+host,uuid:()=> 'request-1',
    api:async(path,body)=>{calls.push({path,body});return{status:'accepted'};},bumpRecency(){},renderSkillPills(){},renderQueue(){},toast(){},saveDraft(){},syncSendButton(){},
  };
  vm.runInNewContext(source.slice(goalStart,goalEnd)+'\n'+source.slice(submitStart,submitEnd),context);
  return{nodes,calls,context};
}

test('/goal submits its objective through the real composer path',async()=>{
  const {nodes,calls,context}=goalComposer();
  nodes.message.value='/goal 完成迁移\n并运行测试';
  await nodes.composer.onsubmit({preventDefault(){}});
  assert.equal(calls.length,1);
  assert.equal(calls[0].body.text,'完成迁移\n并运行测试');
  assert.equal(calls[0].body.workMode,'goal');
  assert.equal(calls[0].body.mode,'send');
  assert.equal(nodes.message.value,'');
  assert.equal(context.workModes.selection,'goal');
});

test('/goal submits on remote hosts when the gateway advertises Goal fallback',async()=>{
  const {nodes,calls}=goalComposer({host:'ssh'});
  nodes.message.value='/goal 完成远端任务';
  await nodes.composer.onsubmit({preventDefault(){}});
  assert.equal(calls.length,1);
  assert.equal(calls[0].path,'/api/sessions/chat/send?host=ssh');
  assert.equal(calls[0].body.workMode,'goal');
});

test('bare /goal only selects Goal and active replies reject the command',async()=>{
  const bare=goalComposer();bare.nodes.message.value='/goal';
  await bare.nodes.composer.onsubmit({preventDefault(){}});
  assert.equal(bare.calls.length,0);
  assert.equal(bare.nodes['work-mode'].value,'goal');
  assert.equal(bare.nodes.message.value,'');
  const active=goalComposer({active:true});active.nodes.message.value='/goal 不应发送';
  await active.nodes.composer.onsubmit({preventDefault(){}});
  assert.equal(active.calls.length,0);
  assert.equal(active.nodes['send-error'].textContent,'当前回复结束后可开启目标模式');
});
