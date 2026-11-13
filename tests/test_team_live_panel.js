'use strict';

const test=require('node:test');
const assert=require('node:assert/strict');
const {TeamLivePanelController,teamMemberRunState,teamGraphRelations,teamSelectionAfterRefresh,teamLiveLayers,teamLiveNodeTasks}=require('../web/team-live-panel.js');

// DOM 测试保留焦点事件与节点身份，覆盖轮询重绘和真实按钮的生命周期。
function installDOM(t){
  const saved={document:global.document,window:global.window,TeamsPanel:global.TeamsPanel,TeamLivePanelController:global.TeamLivePanelController,BridgeI18n:global.BridgeI18n,appendTeamGraphNodeContent:global.appendTeamGraphNodeContent};
  const doc={activeElement:null},ids=new Map();
  class Node{
    constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.style={};this.attributes={};this.className='';this.listeners={};this.textContent='';}
    append(...nodes){for(const node of nodes){if(node.parent)node.parent.children=node.parent.children.filter(child=>child!==node);node.parent=this;this.children.push(node);}}
    replaceChildren(...nodes){if(this.contains(doc.activeElement))doc.activeElement=doc.body;for(const node of this.children)node.parent=null;this.children=[];this.append(...nodes);}
    replaceWith(node){if(!this.parent)return;const parent=this.parent,index=parent.children.indexOf(this);parent.children[index]=node;node.parent=parent;this.parent=null;}
    contains(node){return node===this||this.children.some(child=>child.contains(node));}
    setAttribute(name,value){this.attributes[name]=String(value);}
    getAttribute(name){return this.attributes[name];}
    addEventListener(name,callback){this.listeners[name]=callback;}
    focus(){if(doc.activeElement===this)return;const previous=doc.activeElement;doc.activeElement=this;previous?.listeners.blur?.();this.listeners.focus?.();}
    querySelector(selector){return this.querySelectorAll(selector)[0]||null;}
    querySelectorAll(selector){
      const match=node=>{if(selector.startsWith('.'))return node.className.split(' ').includes(selector.slice(1));const attr=/^\[data-([\w-]+)(?:="([^"]*)")?\]$/.exec(selector);if(attr){const key=attr[1].replace(/-([a-z])/g,(_,letter)=>letter.toUpperCase());return attr[2]===undefined?key in node.dataset:node.dataset[key]===attr[2];}return node.tag===selector;};
      return this.children.flatMap(node=>[...(match(node)?[node]:[]),...node.querySelectorAll(selector)]);
    }
    get classList(){const node=this;return{toggle(name,enabled){const names=new Set(node.className.split(' ').filter(Boolean));if(enabled)names.add(name);else names.delete(name);node.className=[...names].join(' ');},add(name){this.toggle(name,true);},remove(name){this.toggle(name,false);}};}
    closest(){return null;}
  }
  doc.createElement=tag=>new Node(tag);doc.createElementNS=(_,tag)=>new Node(tag);doc.body=new Node('body');doc.activeElement=doc.body;
  doc.getElementById=id=>{if(!ids.has(id))ids.set(id,new Node('div'));return ids.get(id);};
  global.document=doc;global.window={matchMedia:()=>({matches:false}),addEventListener(){}};
  global.TeamsPanel=class{stopPolling(){}};global.TeamLivePanelController=TeamLivePanelController;
  global.BridgeI18n={t:value=>value};global.appendTeamGraphNodeContent=require('../web/team.js').appendTeamGraphNodeContent;
  t.after(()=>Object.assign(global,saved));
  return{doc,root:new Node('div')};
}

function nodeText(node){return node.textContent+node.children.map(nodeText).join('');}

test('issue board groups the same items as canvas and removes deleted items after refresh',async t=>{
  const{root}=installDOM(t),{TeamCanvasController}=require('../web/team-canvas.js');
  let issues=[{id:'a',title:'Implement',status:'in_progress'},{id:'b',title:'Review',status:'waiting'},{id:'c',title:'Delivered',status:'resolved'}];
  const calls=[],controller=new TeamCanvasController({root,request:async path=>{calls.push(path);return path.endsWith('/issues')?{issues}:{inputs:[]};}});
  controller.current={id:'team'};controller.view='board';controller.summary={};controller.boardRoot=document.createElement('div');
  controller.renderIssue=issue=>{const card=document.createElement('button');card.dataset.issueId=issue.id;return card;};
  await controller.refresh(true);
  assert.equal(controller.issues.length,3);assert.equal(controller.boardRoot.querySelectorAll('button').length,3);
  const columns=controller.boardRoot.querySelectorAll('section');
  assert.equal(columns.find(n=>n.dataset.stage==='waiting').querySelector('button').dataset.issueId,'b');
  issues=issues.filter(issue=>issue.id!=='b');await controller.refresh(true);
  assert.deepEqual(controller.boardRoot.querySelectorAll('button').map(n=>n.dataset.issueId),['a','c']);
  assert.ok(calls.every(path=>!path.endsWith('/runs')),'看板不得从历史 Run 重建已删除事项');
});

test('issue board collapses historical items by default',t=>{
  const{root}=installDOM(t),{TeamCanvasController}=require('../web/team-canvas.js');
  const controller=new TeamCanvasController({root,request:async()=>({})});
  controller.boardRoot=document.createElement('div');
  controller.issues=[{id:'current',status:'open'},{id:'old',status:'resolved',historical:true}];
  controller.renderIssue=issue=>{const card=document.createElement('button');card.dataset.issueId=issue.id;return card;};
  controller.renderBoard();
  const history=controller.boardRoot.querySelector('.team-issue-history');
  assert.ok(history);assert.equal(history.open,undefined);
  assert.equal(controller.boardRoot.querySelector('.team-issue-board-grid').querySelectorAll('button').length,1);
  assert.equal(history.querySelectorAll('button').length,1);
});

test('member state prioritizes a failed task while preserving other running work',()=>{
  const member={id:'dev',role:'member'};
  const run={tasks:[
    {id:'running-1',memberId:'dev',status:'running'},
    {id:'failed',memberId:'dev',status:'failed'},
    {id:'running-2',memberId:'dev',status:'dispatching'},
  ]};
  const state=teamMemberRunState(member,run);
  assert.equal(state.state,'attention');assert.equal(state.task.id,'failed');
  assert.equal(state.runningCount,2);
  run.tasks[0].status='completed';assert.equal(teamMemberRunState(member,run).task.id,'failed');
  run.pendingQuestions=[{taskId:'running-2',status:'open'}];
  assert.equal(teamMemberRunState(member,{tasks:[run.tasks[2]],pendingQuestions:run.pendingQuestions}).task.id,'running-2','待答问题归入需处理');
});

test('member state distinguishes blocked, ready, completed and unknown tasks',()=>{
  const member={id:'dev'};
  const run={tasks:[
    {id:'parent',memberId:'lead',status:'running'},
    {id:'child',memberId:'dev',status:'planned',dependsOn:['parent']},
  ]};
  assert.equal(teamMemberRunState(member,run).state,'waiting');
  run.tasks[0].status='completed';assert.equal(teamMemberRunState(member,run).state,'ready');
  run.tasks[1].status='completed';assert.equal(teamMemberRunState(member,run).state,'completed');
  run.tasks[1].status='future_status';assert.equal(teamMemberRunState(member,run).state,'attention');
  assert.equal(teamMemberRunState({id:'lead',role:'lead'},{status:'reducing',tasks:[]}).state,'running');
});

test('graph relations include complete upstream and downstream chains and tolerate cycles',()=>{
  const nodes=[
    {id:'a',dependsOn:['c'],memberIds:['lead']},
    {id:'b',dependsOn:['a'],memberIds:['dev']},
    {id:'c',dependsOn:['b'],memberIds:['review']},
    {id:'d',dependsOn:['b'],memberIds:['dev']},
    {id:'outside',dependsOn:[],memberIds:['other']},
  ];
  const relations=teamGraphRelations(nodes,{kind:'task',id:'b'});
  assert.deepEqual([...relations.selected],['b']);
  assert.deepEqual([...relations.upstream].sort(),['a','c']);
  assert.deepEqual([...relations.downstream].sort(),['a','c','d'],'环上的节点可同时属于上下游，且遍历必须终止');
  assert.equal(relations.related.has('outside'),false);
  const member=teamGraphRelations(nodes,{kind:'member',id:'dev'});
  assert.deepEqual([...member.selected].sort(),['b','d']);
  assert.equal(member.upstream.has('a'),true);
  assert.equal(teamSelectionAfterRefresh({kind:'task',id:'missing'},nodes),null);
  assert.deepEqual(teamSelectionAfterRefresh({kind:'member',id:'dev'},nodes),{kind:'member',id:'dev'},'成员部分任务消失仍保留选择');
  assert.equal(teamSelectionAfterRefresh({kind:'member',id:'other'},nodes.slice(0,4)),null);
});

test('layer fallback keeps cycles bounded and preserves original node order',()=>{
  const nodes=[{id:'a',dependsOn:['b']},{id:'b',dependsOn:['a']},{id:'c',dependsOn:['missing']}];
  const layers=teamLiveLayers(nodes);
  assert.equal(layers.length,2);assert.deepEqual(layers[0].map(node=>node.id),['c']);
  assert.deepEqual(layers[1].map(node=>node.id),['a','b']);
});

test('graph 404 falls back to run tasks and same-version updates do not refetch',async()=>{
  const detail={id:'run',version:3,status:'running',tasks:[{id:'task',memberId:'dev',status:'running',prompt:'实现'}]};
  const calls=[];
  const controller=new TeamLivePanelController({root:{replaceChildren(){}},request:async path=>{
    calls.push(path);
    if(path.endsWith('/graph'))throw Object.assign(Error('not found'),{status:404});
    return detail;
  }});
  controller.render=()=>{};
  await controller.open({team:{id:'team',members:[{id:'dev'}]},run:{id:'run',version:3,status:'running'}});
  assert.equal(calls.length,2);assert.equal(controller.graphError,'');assert.equal(controller.nodes()[0].label,'实现');
  await controller.update({team:{id:'team',members:[{id:'dev'}]},run:detail});
  assert.equal(calls.length,2,'同一版本的全量 detail 只更新展示，不重复请求');
});

test('closing a detail invalidates both run and graph responses',async()=>{
  let resolveDetail,resolveGraph;
  const detail=new Promise(resolve=>{resolveDetail=resolve;}),graph=new Promise(resolve=>{resolveGraph=resolve;});
  const controller=new TeamLivePanelController({root:{replaceChildren(){}},request:path=>path.endsWith('/graph')?graph:detail});
  controller.render=()=>{};
  const opening=controller.open({team:{id:'team'},run:{id:'run'}});
  controller.close();resolveDetail({id:'run',tasks:[]});resolveGraph({nodes:[]});await opening;
  assert.equal(controller.detail,null);assert.equal(controller.graph,null);assert.equal(controller.run,null);
});

test('an active current run only opens explicitly or when restoring an existing detail',()=>{
  const {TeamCanvasController}=require('../web/team-canvas.js');
  const previousDocument=global.document,button={dataset:{}};
  global.document={getElementById:()=>button};
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{t:value=>value,current:{id:'team'},detailRun:null,restoreDetail:null});
  let opened=0;controller.openCurrentRun=()=>{opened++;controller.restoreDetail=null;};
  try{
    controller.setCurrentRun({id:'run',status:'running'},{id:'team'});
    assert.equal(opened,0,'活跃 Run 出现不能自动展开详情');assert.equal(button.disabled,false);
    controller.restoreDetail={mode:'current',runId:'run'};
    controller.setCurrentRun({id:'run',status:'running'},{id:'team'});
    assert.equal(opened,1,'只有返回已打开详情才恢复');
  }finally{global.document=previousDocument;}
});

test('task session resolution shares result, dispatch, thread and member priority',()=>{
  const {teamTaskSessionId}=require('../web/team.js');
  const member={sessionId:'member'},detail={threads:[{id:'thread',providerSessionId:'provider'}]};
  const task={executionThreadId:'thread',dispatchSessionId:'dispatch',result:{sessionId:'result'}};
  assert.equal(teamTaskSessionId(task,detail,member),'result');
  delete task.result;assert.equal(teamTaskSessionId(task,detail,member),'dispatch');
  delete task.dispatchSessionId;assert.equal(teamTaskSessionId(task,detail,member),'provider');
  delete task.executionThreadId;assert.equal(teamTaskSessionId(task,detail,member),'member');
  assert.equal(teamTaskSessionId(null,detail,member),'member');
});

test('focus and pointer previews preserve task buttons and terminate during refresh',t=>{
  const{root,doc}=installDOM(t),controller=new TeamLivePanelController({root});
  controller.team={id:'team',members:[]};controller.run=controller.detail={id:'run',tasks:[{id:'a',status:'running'},{id:'b',status:'planned',dependsOn:['a']}]};
  let renders=0;const render=controller.render.bind(controller);
  controller.render=()=>{assert.ok(++renders<5,'恢复焦点不能再次重绘');render();};controller.render();
  const task=root.querySelector('[data-live-task="a"]');task.focus();
  assert.equal(controller.focusTaskId,'a');assert.equal(doc.activeElement,task);
  assert.equal(root.querySelector('[data-live-task="a"]'),task,'预览不替换按钮');
  assert.equal(renders,1);assert.match(nodeText(root.querySelector('.team-live-selection')),/running/);
  controller.render();assert.equal(doc.activeElement.dataset.liveTask,'a');assert.equal(renders,2);
  doc.body.focus();controller.preview('b','hover');
  assert.equal(root.querySelector('[data-live-task="b"]').className.includes('selected'),true);
  controller.clearSelection();assert.equal(controller.effectiveSelection(),null);assert.equal(renders,2);
});

test('live workflow nodes share run graph content and keep task selection',t=>{
  const{root}=installDOM(t),controller=new TeamLivePanelController({root});
  controller.team={id:'team',members:[{id:'dev',name:'Dev'}]};
  controller.run=controller.detail={id:'run',tasks:[{id:'task',memberId:'dev',status:'completed',prompt:'Implement'}]};
  controller.graph={nodes:[{id:'task',label:'Implement',status:'completed',memberIds:['dev'],taskIds:['task'],runCount:2,resultHead:'Finished output',truncated:true}]};
  controller.render();
  const card=root.querySelector('[data-live-task="task"]');
  assert.ok(card.className.includes('team-graph-node'));
  assert.equal(card.querySelector('.team-graph-node-head').querySelector('strong').textContent,'Implement');
  assert.equal(card.querySelector('.team-graph-runs').textContent,'×2');
  assert.equal(card.querySelector('.team-graph-kind').textContent,'Dev');
  assert.equal(card.querySelector('.team-badge').textContent,'已完成');
  assert.equal(card.querySelector('.task-instructions').textContent,'Finished output');
  assert.equal(card.querySelector('.team-truncated').textContent,'已截断');
  card.listeners.click();assert.deepEqual(controller.pinned,{kind:'task',id:'task'});
});

test('broadcast and repair stages expose their actual execution sessions and anchors',t=>{
  const{root}=installDOM(t),opened=[],located=[];
  const controller=new TeamLivePanelController({root,openSession:(...args)=>opened.push(args),openRun:(...args)=>located.push(args)});
  controller.team={id:'team',members:[{id:'dev',sessionId:'dev-session'},{id:'lead',sessionId:'lead-session'}]};
  controller.run=controller.detail={id:'run',tasks:[{id:'scan-dev',memberId:'dev',status:'running',prompt:'Code'},{id:'scan-lead',memberId:'lead',status:'completed',prompt:'Plan'}]};
  controller.graph={nodes:[{id:'scan',label:'Scan',status:'running',memberIds:['dev','lead'],taskIds:['scan-dev','scan-lead'],runCount:1}]};
  controller.render();controller.select('task','scan');
  let buttons=root.querySelector('.team-live-selection').querySelectorAll('button');assert.equal(buttons.length,4);
  buttons[0].listeners.click();buttons[1].listeners.click();buttons[2].listeners.click();buttons[3].listeners.click();
  assert.deepEqual(opened.map(([id,source])=>[id,source.taskId]),[['dev-session','scan-dev'],['lead-session','scan-lead']]);
  assert.deepEqual(located.map(([,id,options])=>[id,options.taskId]),[['run','scan-dev'],['run','scan-lead']]);
  controller.run=controller.detail={id:'repair',tasks:[{id:'lead',memberId:'lead',status:'completed'},{id:'actfix-lead-1',memberId:'lead',status:'running',prompt:'Repair'}]};
  controller.graph={nodes:[{id:'lead',status:'running',memberIds:['lead'],taskIds:['lead','actfix-lead-1'],runCount:2}]};
  controller.render();controller.select('task','lead');
  assert.equal(root.querySelector('[data-live-task="lead"]').dataset.status,'running');
  buttons=root.querySelector('.team-live-selection').querySelectorAll('button');assert.equal(buttons.length,2);buttons[1].listeners.click();
  assert.equal(located.at(-1)[2].taskId,'actfix-lead-1');
  assert.deepEqual(teamLiveNodeTasks(controller.graph.nodes[0],controller.detail).map(task=>task.id),['lead','actfix-lead-1']);
});

test('failed graph refresh discards stale topology and uses the latest task snapshot',async t=>{
  const{root}=installDOM(t),detail={id:'run',version:2,tasks:[{id:'a',status:'completed'},{id:'b',status:'running',dependsOn:['a']}]};
  const controller=new TeamLivePanelController({root,request:async path=>{if(path.endsWith('/graph'))throw Error('offline');return detail;}});
  controller.team={id:'team',members:[]};controller.run={id:'run',version:1};controller.graph={nodes:[{id:'a',status:'running',taskIds:['a']}]};
  await controller.load();
  assert.equal(controller.graph,null);assert.equal(controller.graphError,'offline');
  assert.deepEqual(controller.nodes().map(node=>node.id),['a','b']);
  controller.select('task','b');assert.match(nodeText(root.querySelector('.team-live-selection')),/前置已满足/);
  controller.graph={nodes:[{id:'broadcast',memberIds:[],status:'running'}]};
  assert.deepEqual(controller.nodes().map(node=>node.id),['a','b'],'旧网关缺少任务映射时安全回退');
});

test('canvas navigation restores details after member sessions and complete run pages',t=>{
  installDOM(t);
  const{TeamCanvasController}=require('../web/team-canvas.js');
  const team={id:'team',members:[]},run={id:'a',tasks:[{id:'task',status:'running'}]};
  const controller=new TeamCanvasController({root:document.createElement('section'),request:async path=>path.endsWith('/graph')?{nodes:[]}:run});
  controller.render=()=>{};controller.refresh=async()=>{};
  t.after(()=>clearInterval(controller.pollTimer));
  controller.open(team);controller.setCurrentRun(run,team);controller.openCurrentRun(null);
  controller.livePanel.pinned={kind:'task',id:'task'};
  controller.open(null);assert.equal(controller.pendingRestore?.teamId,team.id);
  controller.open(team);controller.setCurrentRun(run,team);
  assert.equal(controller.detail.hidden,false);assert.equal(controller.detailRun.id,'a');
  assert.deepEqual(controller.livePanel.pinned,{kind:'task',id:'task'});
  controller.open(team);controller.setCurrentRun(run,team);
  assert.equal(controller.detail.hidden,false,'完整 Run 返回不能被 setView 再次清理');
  assert.deepEqual(controller.livePanel.pinned,{kind:'task',id:'task'});
  controller.setCurrentRun({id:'b',tasks:[{id:'new-task',status:'planned'}]},team);
  assert.equal(controller.detailRun.id,'b');assert.equal(controller.livePanel.run.id,'b');
  assert.equal(controller.livePanel.pinned,null,'切换当前 Run 清理旧任务选择');
  controller.liveRoot.querySelector('[data-live-task="new-task"]').focus();
  controller.setCurrentRun({id:'b',tasks:[{id:'new-task',status:'running'}]},team);
  assert.equal(document.activeElement.dataset.liveTask,'new-task','完整画布详情重绘也必须恢复任务焦点');
});
