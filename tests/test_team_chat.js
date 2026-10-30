'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
global.t_safe=text=>text;
global.teamRunStatusLabel=status=>status;
global.teamTaskStatusLabel=status=>status;
global.TEAM_TERMINAL_RUN_STATUSES=new Set(['completed','failed','cancelled','stopped','outcome_unknown']);
global.BridgeI18n={agentLabel:kind=>kind};
const {TeamChatController,TeamSettingsDialog,TeamLeadPicker,teamSubmissionFields,teamWorkflowStages}=require('../web/team-chat.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.style={};this.hidden=false;this.className='';this.textContent='';this.dataset={};this.listeners={};}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(type,listener){(this.listeners[type]||(this.listeners[type]=[])).push(listener);}
  showModal(){this.open=true;}
  close(){this.open=false;for(const listener of this.listeners.close||[])listener();}
  get classList(){
    const node=this;
    return{add(name){if(!node.className.split(/\s+/).includes(name))node.className+=(node.className?' ':'')+name;},toggle(name,force){node.className=node.className.split(/\s+/).filter(Boolean).filter(part=>part!==name).concat(force===false?[]:[name]).join(' ');}};
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
  assert.match(text(find(root,'team-workflow-member')[1]),/前端 · zcode/);
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
    if(path==='/api/projects/acp%3Aproject/agents')return{agents:[{agent:'claude',sessionId:'sess-new',title:'只读排查最近交互项目排序'}]};
    if(path==='/api/teams/team-1/members'){
      added=options.body;
      return{...team,members:[...team.members,{id:added.id,sessionId:'sess-new',agentKind:'claude',name:'claude',role:added.role}]};
    }
    throw new Error('unexpected request '+path);
  },onChange:updated=>{changed=updated;}});
  await settings.open(team);
  assert.equal(settings._dialog.open,true);
  assert.equal(find(settings.body,'team-role-select').length,2);
  assert.equal(find(settings.body,'team-add-agent')[0].children[0].textContent,'claude');
  await settings.addMember('claude','reviewer');
  assert.equal(added.id,'member-claude');
  assert.equal(added.role,'reviewer');
  assert.equal(added.version,2);
  assert.equal(changed.members.length,3);
});

test('team settings allows the lead to also be the default reviewer',async()=>{
  const team={...teams[0],project:'acp:project',version:2};
  let patch=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/projects/acp%3Aproject/agents')return{agents:[]};
    if(path==='/api/teams/team-1'&&options.method==='PATCH'){
      patch=options.body;return{...team,reviewerId:patch.reviewerId,version:3};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const reviewer=find(settings.body,'team-role-select')[1];
  assert.deepEqual(reviewer.children.map(option=>option.value),['','lead','dev']);
  reviewer.value='lead';reviewer.onchange();await flush();
  assert.deepEqual(patch,{reviewerId:'lead',version:2});
  assert.equal(settings.team.reviewerId,'lead');
  assert.equal(find(settings.body,'team-member-role')[0].textContent,'主控 · 评审');
});

test('team without runs shows a ready empty state instead of a blank panel',async()=>{
  const root=new FakeNode('div');
  const controller=new TeamChatController({root,request:async()=>({runs:[]}),openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(root.hidden,false);
  assert.equal(find(root,'team-workflow-empty').length,1);
  assert.match(text(root),/团队已就绪/);
});

test('first team lead picker returns the selected agent kind',async()=>{
  const calls=[];
  const picker=new TeamLeadPicker({request:async(path,options={})=>{
    calls.push({path,options});
    return{agents:[{agent:'kimi',title:'旧会话一'},{agent:'zcode',title:'旧会话二'}]};
  }});
  const selected=picker.open({key:'acp:project',name:'项目'});await flush();
  assert.equal(picker._dialog.open,true);
  assert.deepEqual(find(picker.body,'team-lead-choice').map(node=>node.textContent),['kimi','zcode']);
  find(picker.body,'team-lead-choice')[1].onclick();
  assert.equal(await selected,'zcode');
  assert.deepEqual(calls.map(call=>call.path),['/api/projects/acp%3Aproject/agents']);
  assert.equal(calls.filter(call=>call.options.method==='PUT').length,0);
});

test('cancelling first team lead picker returns null without creating a team',async()=>{
  const calls=[];
  const picker=new TeamLeadPicker({request:async(path,options={})=>{
    calls.push({path,options});
    return{agents:[{agent:'kimi',title:'Kimi'}]};
  }});
  const selected=picker.open({key:'acp:project',name:'项目'});await flush();
  find(picker.body,'plain')[0].onclick();
  assert.equal(await selected,null);
  assert.equal(calls.filter(call=>call.options.method==='PUT').length,0);
});

test('team submission fields distinguish direct chat from orchestration',()=>{
  assert.deepEqual(teamSubmissionFields('direct','team-1'),{teamMode:'direct'});
  assert.deepEqual(teamSubmissionFields('team','team-1'),{teamMode:'team',teamId:'team-1'});
});

test('pending delivery accepts a result through the delivery API',async()=>{
  const root=new FakeNode('div'),calls=[];
  const run={id:'run-1',status:'completed',deliveryStatus:'pending_review',version:3,tasks:[]};
  const controller=new TeamChatController({root,request:async(path,options={})=>{
    calls.push({path,options});
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    if(path.endsWith('/delivery'))return{...run,deliveryStatus:'accepted',version:4};
    return run;
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(find(root,'team-delivery-actions').length,1);
  find(root,'team-delivery-actions')[0].children[0].onclick();await flush();
  const delivery=calls.find(call=>call.path.endsWith('/delivery'));
  assert.equal(delivery.options.method,'POST');
  assert.deepEqual(delivery.options.body,{status:'accepted',version:3});
  assert.equal(controller.run.deliveryStatus,'accepted');
  assert.equal(find(root,'team-delivery-actions').length,0);
});

test('delivery card renders the four evidence dimensions separately',async()=>{
  const run={id:'run-1',status:'completed',deliveryStatus:'pending_review',version:2,tasks:[],delivery:{
    deliveryStatus:'pending_review',
    agent:{completed:2,total:3},
    resultSummary:{text:'t1：接口已实现；t2：用例补齐',truncated:false},
    review:{taskId:'rev',verdict:'passed'},
    verify:{configured:true,status:'failed',exitCode:1},
    pendingQuestions:1
  }};
  const {root}=setup(run);
  const controller=new TeamChatController({root,request:async path=>{
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    return run;
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  const dims=find(root,'team-delivery-dim');
  assert.equal(dims.length,3);
  assert.match(dims[0].textContent,/Agent：2\/3 已完成/);
  assert.match(dims[1].textContent,/Review：已通过/);
  assert.match(dims[2].textContent,/验证：未通过/);
  assert.match(text(find(root,'team-delivery-summary')[0]),/接口已实现/);
  // 未参与评审 / 未配置闸门的降级展示。
  const bare={id:'run-2',status:'completed',deliveryStatus:'pending_review',version:1,tasks:[],delivery:{
    deliveryStatus:'pending_review',agent:{completed:1,total:1},
    resultSummary:{text:'',truncated:true},verify:{configured:false},pendingQuestions:0
  }};
  const root2=new FakeNode('div');
  const controller2=new TeamChatController({root:root2,request:async path=>{
    if(path.endsWith('/runs'))return{runs:[{id:'run-2'}]};
    return bare;
  },openRun(){}});
  controller2.setTeams(teams);controller2.open(teams[0]);await flush();
  const dims2=find(root2,'team-delivery-dim');
  assert.match(dims2[1].textContent,/Review：未参与/);
  assert.match(dims2[2].textContent,/验证：未配置/);
  assert.equal(find(root2,'team-delivery-summary').length,0);
});

test('workflow stages fan out parallel workers before the merge and final reduction',()=>{
  const stages=teamWorkflowStages([
    {id:'lead'},
    {id:'lead-d1-0',dependsOn:['lead']},
    {id:'lead-d1-1',dependsOn:['lead']},
    {id:'lead-d1-2',dependsOn:['lead-d1-0','lead-d1-1']},
    {id:'lead-auto-reduce',dependsOn:['lead','lead-d1-0','lead-d1-1','lead-d1-2']},
  ]);
  assert.deepEqual(stages.map(stage=>stage.map(task=>task.id)),[
    ['lead'],['lead-d1-0','lead-d1-1'],['lead-d1-2'],['lead-auto-reduce'],
  ]);
});

test('legacy delegate tasks without persisted dependencies get the same display topology',()=>{
  const stages=teamWorkflowStages([
    {id:'lead',memberId:'kimi'},
    {id:'lead-d1-0',memberId:'claude'},
    {id:'lead-d1-1',memberId:'dsh'},
    {id:'lead-d1-2',memberId:'kimi'},
    {id:'lead-auto-reduce',memberId:'kimi',dependsOn:['lead','lead-d1-0','lead-d1-1','lead-d1-2']},
  ]);
  assert.deepEqual(stages.map(stage=>stage.map(task=>task.id)),[
    ['lead'],['lead-d1-0','lead-d1-1'],['lead-d1-2'],['lead-auto-reduce'],
  ]);
});

test('workflow station title prefers the prompt over the internal id',()=>{
  const {teamWorkflowTaskTitle}=require('../web/team-chat.js');
  // 任务 id 是内部标识（lead-d1-0），轨道上要展示提示词首行。
  assert.equal(teamWorkflowTaskTitle({id:'lead-d1-0',instructionsHead:'## 重构登录接口\n补充测试'}),'重构登录接口');
  assert.equal(teamWorkflowTaskTitle({id:'lead-d1-1',prompt:'## 只读排查\n细节'}),'只读排查');
  // 提示词开头的绝对路径在窄列里只会占满两行，展示时去掉，正文交给 CSS 两行截断。
  assert.equal(teamWorkflowTaskTitle({id:'t',prompt:'在 /Users/petaflops/works/unsorted/mobile-bridge 只读排查 internal/httpd'}),
    '只读排查 internal/httpd');
  assert.equal(teamWorkflowTaskTitle({id:'t',prompt:'一'.repeat(60)}),'一'.repeat(60));
  // 超长英文标识（测试名、路径段）在窄列里会被折在词中间，显示时中截，原文仍在 title。
  assert.equal(teamWorkflowTaskTitle({id:'t',prompt:'排查 TestTeamIntegration2IdempotencyAndRestart 的失败'}),
    '排查 TestTeamIntegr…AndRestart 的失败');
  // 主控站点保留角色名，没有提示词时回落到任务 id。
  assert.equal(teamWorkflowTaskTitle({id:'lead',prompt:'汇总成果'}),'负责人');
  assert.equal(teamWorkflowTaskTitle({id:'custom'}),'custom');
});

test('workflow counts come from the tasks, not from the run status',()=>{
  const {teamWorkflowCounts}=require('../web/team-chat.js');
  const counts=teamWorkflowCounts([
    {status:'completed'},{status:'failed'},{status:'outcome_unknown'},
    {status:'running'},{status:'ready'},{status:'stopped'},{status:'planned'},
  ]);
  assert.deepEqual(counts,{total:7,completed:1,failed:2,running:2,stopped:1,pending:1});
  assert.deepEqual(teamWorkflowCounts([]),{total:0,completed:0,failed:0,running:0,stopped:0,pending:0});
});

test('workflow outcome never reports a run with failed tasks as completed',()=>{
  const {teamWorkflowOutcome}=require('../web/team-chat.js');
  const failed={total:3,failed:1,completed:2,running:0,stopped:0,pending:0};
  // Run 的 completed 只表示编排走完，任务里有失败就必须说「部分失败」。
  assert.deepEqual(teamWorkflowOutcome({status:'completed'},failed),{label:'部分失败',tone:'bad'});
  assert.deepEqual(teamWorkflowOutcome({status:'completed'},{total:3,failed:0,stopped:0}),
    {label:'已完成',tone:'ok'});
  assert.deepEqual(teamWorkflowOutcome({status:'failed'},{total:3,failed:2}),{label:'已失败',tone:'bad'});
  assert.deepEqual(teamWorkflowOutcome({status:'stopped'},{total:3,failed:0}),{label:'已停止',tone:'idle'});
  assert.deepEqual(teamWorkflowOutcome({status:'collecting'},{total:3,failed:0}),{label:'collecting',tone:'warn'});
  assert.deepEqual(teamWorkflowOutcome({status:'outcome_unknown'},{total:1,failed:1}),{label:'结果未知',tone:'bad'});
});

test('workflow member label does not say the agent name twice',()=>{
  const {teamWorkflowMemberLabel}=require('../web/team-chat.js');
  assert.equal(teamWorkflowMemberLabel({name:'kimi',agentKind:'kimi'}),'kimi');
  assert.equal(teamWorkflowMemberLabel({name:'前端',agentKind:'zcode'}),'前端 · zcode');
  assert.equal(teamWorkflowMemberLabel({id:'member-claude',agentKind:'claude'}),'member-claude · claude');
  assert.equal(teamWorkflowMemberLabel({}, {memberId:'member-1'}),'member-1');
  assert.equal(teamWorkflowMemberLabel({agentKind:'kimi'}),'kimi');
});
