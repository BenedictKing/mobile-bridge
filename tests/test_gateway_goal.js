'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const {WorkModes}=require('../web/modes.js');

class Node {
  constructor(tag='div') {this.tag=tag;this.children=[];this.hidden=false;this.disabled=false;this.value='';this.textContent='';this.open=false;this.dataset={};this.attributes={};this.classList={toggle(){}};}
  append(...children){this.children.push(...children);}
  replaceChildren(...children){this.children=[...children];}
  get firstElementChild(){return this.children[0];}
  querySelector(){return this.option||null;}
  querySelectorAll(){return this.items||[];}
  setAttribute(name,value){this.attributes[name]=value;}
  focus(){}
}

function setup(provider='gateway'){
  const values=new Map(),calls=[];
  const storage={getItem:key=>values.get(key)||null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};
  const select=new Node('select');select.option=new Node('option');
  const items=['default','plan','goal'].map(workMode=>Object.assign(new Node('button'),{dataset:{workMode}}));
  const control=new Node('div');control.items=items;
  const root=new Node(),toggle=new Node('button');
  global.document={createElement:tag=>new Node(tag)};
  global.BridgeI18n={t:value=>value,locale:()=>"zh-CN"};
  const modes=new WorkModes({select,control,goalRoot:root,goalToggle:toggle,storage,newId:()=>"op-1",
    onStatus:async payload=>calls.push(payload),onCancel:async()=>{},onEdit:async()=>{},onContinue:async()=>{},onComplete:async()=>{},onReview:async payload=>calls.push(payload)});
  modes.open('chat');
  modes.render({host:'local',status:'idle',capabilities:['goals'],goalRuntimeAvailable:true,
    goal:{provider,goalId:'goal-1',objective:'整理仓库',status:'waiting_review',revision:7,autoStepsUsed:1,autoStepsLimit:5}},'send',false);
  return {modes,root,calls,select,items};
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
  const actions=root.firstElementChild.children.find(node=>node.className==='goal-actions');
  assert.ok(actions);
  assert.ok(actions.children.some(node=>node.textContent==='人工继续一轮'));
  assert.ok(actions.children.some(node=>node.textContent==='标记完成'));
});

test('unknown delivery requires an explicit verified review action',async()=>{
  const {modes,root,calls}=setup();
  modes.render({...modes.view,goal:{...modes.view.goal,status:'outcome_unknown'}},'send',false);
  const actions=root.firstElementChild.children.find(node=>node.className==='goal-actions');
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

test('segmented mode control keeps selection and exposes gateway Goal on remote hosts without helper copy',()=>{
  const {modes,select,items}=setup();
  modes.render({host:'ssh',status:'idle',capabilities:['goals'],goalRuntimeAvailable:true},'send',false);
  assert.equal(items[2].hidden,false);
  assert.equal(items[2].disabled,false);
  items[1].onclick();
  assert.equal(modes.storage.getItem('work-mode:chat'),'plan');
  assert.equal(select.value,'plan');
  assert.equal(items[1].attributes['aria-pressed'],'true');
  assert.equal(items[1].title,'');
  assert.equal(items[2].title,'');
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


test('Goal starts collapsed and expands to its objective and controls',()=>{
  const {modes,root}=setup(),details=root.firstElementChild;
  assert.equal(details.tag,'details');
  assert.equal(details.open,false);
  assert.equal(root.children.some(node=>node.className==='goal-actions'),false);
  assert.equal(details.children[1].className,'goal-objective');
  assert.equal(details.children[1].textContent,'整理仓库');
  assert.ok(details.children.some(node=>node.className==='goal-actions'));
  details.open=true;
  modes.render(modes.view,'send',false);
  assert.equal(root.firstElementChild.open,true);
  assert.equal(root.firstElementChild.children[1].textContent,'整理仓库');
});

test('Goal status cannot be dismissed until it reaches a terminal state',()=>{
  const {modes,root}=setup();
  for(const status of ['active','dispatching','evaluating','waiting_input','waiting_review','outcome_unknown','paused','blocked','usageLimited','budgetLimited']){
    modes.render({...modes.view,goal:{...modes.view.goal,status}},'send',false);
    assert.equal(root.children.some(node=>node.className==='goal-close'),false,status);
  }
  for(const status of ['completed','cancelled','complete']){
    modes.render({...modes.view,goal:{...modes.view.goal,status}},'send',false);
    assert.equal(root.children.some(node=>node.className==='goal-close'),true,status);
  }
});

test('pending goal creation cannot be dismissed before confirmation',()=>{
  const {modes,root}=setup();
  modes.render({...modes.view,goal:null,goalSubmission:{id:'request-1',status:'pending'}},'send',false);
  assert.equal(root.children.some(node=>node.className==='goal-close'),false);
  modes.render({...modes.view,goal:null,goalSubmission:{id:'request-1',status:'unknown'}},'send',false);
  assert.equal(root.children.some(node=>node.className==='goal-close'),true);
});

test('running composer sends through the queue without a send-mode menu',async()=>{
  const {nodes,calls}=goalComposer({active:true});
  nodes.message.value='继续处理';
  await nodes.composer.onsubmit({preventDefault(){}});
  assert.equal(calls.length,1);
  assert.equal(calls[0].body.mode,'queue');
  assert.equal(nodes['send-mode'].value,'queue');
  const html=readFileSync(require.resolve('../web/index.html'),'utf8');
  assert.equal(html.includes('id="send-menu-button"'),false);
  assert.equal(html.includes('id="send-mode-menu"'),false);
  assert.match(html,/<span id="send-label">发送<\/span>/);
});
