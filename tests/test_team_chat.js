'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
global.t_safe=text=>text;
global.teamRunStatusLabel=status=>status;
global.teamTaskStatusLabel=status=>status;
global.TEAM_TERMINAL_RUN_STATUSES=new Set(['completed','failed','cancelled','stopped','outcome_unknown']);
global.BridgeI18n={agentLabel:kind=>kind};
const {TeamChatController,TeamSettingsDialog}=require('../web/team-chat.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.hidden=false;this.className='';this.textContent='';this.dataset={};}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  setAttribute(name,value){this.attributes[name]=value;}
  showModal(){this.open=true;}
  close(){this.open=false;}
  get classList(){
    const node=this;
    return{toggle(name,force){node.className=node.className.split(/\s+/).filter(Boolean).filter(part=>part!==name).concat(force===false?[]:[name]).join(' ');}};
  }
}
global.Option=class{constructor(text,value){this.textContent=text;this.value=value;this.className='';this.children=[];}};
global.document={createElement:tag=>new FakeNode(tag),body:new FakeNode('body')};
function find(node,cls){const rows=[];const visit=current=>{if(current.className.split(/\s+/).includes(cls))rows.push(current);for(const child of current.children)visit(child);};visit(node);return rows;}
function text(node){return node.textContent+node.children.map(text).join('');}
function flush(){return new Promise(resolve=>setImmediate(resolve));}

const teams=[{id:'team-1',name:'食材团队',leadId:'lead',members:[
  {id:'lead',sessionId:'sess-lead',agentKind:'kimi',name:'负责人'},
  {id:'dev',sessionId:'sess-dev',agentKind:'zcode',name:'前端'},
]}];

function setup(runDetail){
  const root=new FakeNode('div'),calls=[],opened=[];
  const controller=new TeamChatController({root,request:async path=>{
    calls.push(path);
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    return runDetail;
  },openRun:(teamId,runId)=>opened.push({teamId,runId})});
  return{root,calls,opened,controller};
}

test('team directory resolves only a unique lead session',()=>{
  const {controller}=setup({tasks:[]});
  controller.setTeams(teams);
  assert.equal(controller.teamById('team-1').name,'食材团队');
  assert.equal(controller.teamForSession('sess-lead').id,'team-1');
  assert.equal(controller.teamForSession('sess-dev'),null);
  controller.setTeams([{...teams[0],project:'acp:old',projectCwd:'/work',defaultForProject:true}]);
  assert.equal(controller.teamForProject({key:'codex:new',cwd:'/work'}).id,'team-1');
  controller.setTeams([...teams,{...teams[0],id:'team-2'}]);
  assert.equal(controller.teamForSession('sess-lead'),null);
});

test('workflow renders run tasks and supports member collapse and details',async()=>{
  const detail={id:'run-1',status:'running',tasks:[
    {id:'lead',memberId:'lead',status:'completed',prompt:'理解目标'},
    {id:'implement',memberId:'dev',status:'running',prompt:'核对首页'},
  ]};
  const{root,controller,opened}=setup(detail);
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(root.hidden,false);
  assert.equal(find(root,'team-workflow-station').length,2);
  assert.match(find(root,'team-workflow-pill')[1].textContent,/前端 · zcode/);
  const toggle=find(root,'team-workflow-head')[0].children[2];
  toggle.onclick();
  assert.equal(find(root,'team-workflow-head')[0].children[2].attributes['aria-expanded'],'false');
  assert.equal(find(root,'team-workflow-card')[0].className.includes('compact'),true);
  const details=find(root,'team-workflow-head')[0].children[3];
  details.onclick();
  assert.deepEqual(opened,[{teamId:'team-1',runId:'run-1'}]);
  controller.close();assert.equal(root.hidden,true);
});

test('team settings adds an agent with role and optimistic version',async()=>{
  const team={...teams[0],project:'acp:project',version:2};
  let added=null,changed=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/projects/acp:project/agents')return{agents:[{agent:'zcode',sessionId:'sess-new',title:'ZCode'}]};
    if(path==='/api/teams/team-1/members'){
      added=options.body;
      return{...team,members:[...team.members,{id:added.id,sessionId:'sess-new',agentKind:'zcode',name:'zcode',role:added.role}]};
    }
    throw new Error('unexpected request '+path);
  },onChange:updated=>{changed=updated;}});
  await settings.open(team);
  assert.equal(settings._dialog.open,true);
  assert.equal(find(settings.body,'team-settings-lead').length,2);
  await settings.addMember('zcode','reviewer');
  assert.equal(added.id,'member-zcode');
  assert.equal(added.role,'reviewer');
  assert.equal(added.version,2);
  assert.equal(changed.members.length,3);
});

test('team without runs shows a ready empty state instead of a blank panel',async()=>{
  const root=new FakeNode('div');
  const controller=new TeamChatController({root,request:async()=>({runs:[]}),openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(root.hidden,false);
  assert.equal(find(root,'team-workflow-empty').length,1);
  assert.match(text(root),/团队已就绪/);
});
