'use strict';
// 团队页（web/team.js）在英文界面下的展示：角色标签、项目名、停靠原因与任务错误的本地化，
// 以及 team.js / team-chat.js 里 t()/t_safe() 包裹的中文字面量都已登记英文词条。
const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
global.localStorage={getItem:()=>'en',setItem(){}};
global.BridgeI18n=require('../web/i18n.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.style={};this.hidden=false;this.className='';this.textContent='';this.dataset={};}
  append(...nodes){this.children.push(...nodes);}
  prepend(...nodes){this.children.unshift(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  setAttribute(name,value){this.attributes[name]=value;if(name==='class')this.className=value;}
  addEventListener(name,handler){this.listeners=this.listeners||{};this.listeners[name]=handler;}
  querySelector(){return null;}
  querySelectorAll(selector){const rows=[];const visit=current=>{for(const child of current.children||[]){if(selector==='*'||child.tag===selector)rows.push(child);visit(child);}};visit(this);return rows;}
  get childNodes(){return this.children;}
  get classList(){const node=this;return{add(name){node.className+=(node.className?' ':'')+name;},remove(){},toggle(){},contains(name){return node.className.split(/\s+/).includes(name);}};}
}
global.document={createElement:tag=>new FakeNode(tag),createElementNS:(namespace,tag)=>new FakeNode(tag),body:new FakeNode('body'),querySelectorAll:()=>[]};
global.BridgeIcons=require('../web/icons.js');
global.dagre=require('../web/vendor/dagre.min.js');
const {TeamsPanel,TeamRunScreen,teamRunStopReasonText,teamGraphLayout}=require('../web/team.js');

test('completed execution with an unconfirmed goal remains actionable',()=>{
  const {teamRunGroup}=require('../web/team.js');
  assert.equal(teamRunGroup({status:'completed',outcome:{status:'pending_confirmation'}}),'attention');
  assert.equal(teamRunGroup({status:'completed',outcome:{status:'satisfied'}}),'terminal');
  assert.equal(teamRunGroup({status:'completed',outcome:{status:'unsatisfied',closed:true}}),'terminal');
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  const row=panel.renderActivityRow({id:'team',name:'Team'},{id:'run',status:'completed',outcome:{status:'partial',label:'部分达成'}});
  assert.equal(row.dataset.group,'attention');
  assert.match(text(row),/Execution ended/);assert.match(text(row),/Partially satisfied/);
});

function find(node,cls){const rows=[];const visit=current=>{if(String(current.className).split(/\s+/).includes(cls))rows.push(current);for(const child of current.children)visit(child);};visit(node);return rows;}
function text(node){return node.textContent+node.children.map(text).join('');}
function feedbackOutcome(overrides={}){
  return{scope:{id:'run',hash:'scope'},evidenceHash:'evidence',executionStatus:'completed',status:'pending_confirmation',label:'结果待确认',reason:'查看结果',canConfirm:true,
    result:{text:'回答已经给出'},contract:{revision:1,verify:{requirement:'auto',status:'not_configured'},review:{requirement:'auto',status:'not_configured'}},...overrides};
}
test('goal results are read-only and send supplementary requirements through chat',()=>{
  const {renderTeamGoalReview}=require('../web/team.js');
  for(const scope of [{id:'run',hash:'scope'},{id:'issue-a',hash:'scope'}]){
    for(const executionStatus of ['completed','dispatching','waiting_input','failed','cancelled']){
      const sent=[],drafts=new Map();
      const panel=renderTeamGoalReview(feedbackOutcome({scope,executionStatus}),{drafts,submit:body=>sent.push(body)});
      assert.match(text(panel),/回答已经给出/);
      assert.match(text(panel),/Send additional requirements in team chat/);
      for(const tag of ['textarea','select','button','fieldset'])assert.equal(panel.querySelectorAll(tag).length,0,tag+' form must be removed');
      assert.equal(drafts.size,0);assert.equal(sent.length,0);
    }
  }
});
test('empty unfinished results remain hidden',()=>{
  const {renderTeamGoalReview}=require('../web/team.js');
  assert.equal(renderTeamGoalReview(feedbackOutcome({executionStatus:'cancelled',result:null})).hidden,true);
});
test('workspace changes preserve the confirmed outcome and show both versions',()=>{
  const {renderTeamGoalReview}=require('../web/team.js');
  const panel=renderTeamGoalReview(feedbackOutcome({status:'satisfied',workspaceChange:{confirmedHead:'1234567890abcdef',currentHead:'abcdef1234567890',reason:'工作区版本已变化'}}));
  assert.match(text(panel),/The workspace changed; the earlier assessment is preserved/);
  assert.match(text(panel),/1234567890ab → abcdef123456/);
});
test('shared output and real verification evidence remain visible',()=>{
  const {renderTeamGoalReview}=require('../web/team.js');
  const panel=renderTeamGoalReview(feedbackOutcome({result:{text:'共享汇总',includesShared:true},contract:{verify:{requirement:'required',status:'failed'},review:{requirement:'auto',status:'not_configured'}}}));
  assert.match(text(panel),/Includes output from shared tasks/);assert.match(text(panel),/Did not pass/);
  assert.equal(panel.querySelectorAll('button').length,0);
});

const team={id:'t1',name:'团队',leadId:'kimi',project:'codex:7015435b2c37',projectCwd:'/Users/me/mobile-bridge/',policy:{},members:[
  {id:'kimi',name:'kimi',role:'lead',agentKind:'kimi'},
  {id:'buddy',name:'workbuddy',role:'member',agentKind:'workbuddy'},
]};

test('project board defaults to issues and keeps run history in a separate execution view',async()=>{
  const root=new FakeNode('div'),calls=[],opened=[];
  const panel=new TeamsPanel({root,teamId:'t1',openRun:(...args)=>opened.push(args),request:async url=>{
    calls.push(url);
    if(url==='/api/teams')return{teams:[team,{...team,id:'t2',name:'别的团队'}]};
    if(url.endsWith('/issues'))return{issues:[{id:'i1',title:'本项目事项',status:'in_progress',progress:{known:true,total:1,completed:0},runs:[{id:'r1',goal:'本项目任务',status:'waiting_input'}]}]};
    return{runs:[{id:'r1',goal:'本项目任务',status:'waiting_input',updatedAt:'2026-11-10T00:00:00Z',taskCounts:{completed:1},issueIds:['i1']}]};
  }});
  await panel.refreshAll();
  assert.deepEqual(calls,['/api/teams','/api/teams/t1/runs?includeClassification=1','/api/teams/t1/issues']);
  assert.equal(find(root,'team-card').length,0,'项目看板不重复呈现团队卡片和跨团队管理');
  assert.ok(!text(root).includes('New team'));
  assert.ok(!text(root).includes('别的团队'));
  assert.equal(text(find(root,'team-kpi')[0]),'1Item');
  assert.equal(find(root,'team-issue-row').length,1);
  assert.equal(find(root,'team-activity-row').length,0,'默认看板不再以 Run 为对象');
  find(root,'team-issue-run-link')[0].onclick();assert.deepEqual(opened,[['t1','r1']]);
  panel.view='records';panel.render();
  assert.equal(find(root,'team-activity-row').length,1);
  panel.filter='active';await panel.refreshAll();assert.equal(panel.filter,'active');
  assert.equal(find(root,'team-activity-row').length,0,'执行记录筛选不能混入其他状态或团队');
});

test('issue board collapses historical items and execution rows label run kinds',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  panel.teams=[team];
  panel.issues.set(team.id,{rows:[{id:'current',title:'当前事项',status:'open',runs:[]},{id:'old',title:'历史事项',status:'resolved',historical:true,runs:[]}]});
  const board=panel.renderIssueActivity();
  const history=find(board,'team-issue-history')[0];
  assert.ok(history);assert.equal(history.open,undefined,'历史事项默认折叠');
  assert.equal(find(board,'team-issue-row').length,2,'当前与历史事项只各出现一次');
  panel.filter='active';
  assert.match(text(panel.renderIssueActivity()),/No items match this filter/,'筛选同时作用于当前和历史事项');
  panel.filter='all';panel.view='records';
  const explicit=panel.renderActivityRow(team,{id:'explicit',status:'completed',autoPlan:false,issueIds:[]});
  assert.match(text(explicit),/Explicit plan/);assert.match(text(explicit),/Unclassified execution/);
  const scheduled=panel.renderActivityRow(team,{id:'scheduled',status:'completed',scheduledFrom:'trigger',issueIds:['current']});
  assert.match(text(scheduled),/Scheduled/);assert.match(text(scheduled),/Linked to 1 items/);
  const classifier=panel.renderActivityRow(team,{id:'classifier',status:'completed',classificationOnly:true,issueIds:[]});
  assert.match(text(classifier),/Classification/);
});

test('plan execution view excludes automatic and scheduled runs',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  panel.teams=[team];panel.view='plans';
  panel.runs.set(team.id,{rows:[
    {id:'automatic',goal:'自动',status:'completed',autoPlan:true},
    {id:'scheduled',goal:'定时',status:'completed',autoPlan:false,scheduledFrom:'trigger'},
    {id:'explicit',goal:'手动计划',status:'completed',autoPlan:false},
  ]});
  const activity=panel.renderActivity();
  assert.equal(find(activity,'team-activity-row').length,1);
  assert.match(text(activity),/Plan executions/);assert.match(text(activity),/手动计划/);
  assert.doesNotMatch(text(activity),/自动|定时/);
  assert.equal(panel.runStats().total,1);
  const tabs=find(panel.renderViewTabs(),'team-view-tab');
  assert.deepEqual(tabs.map(text),['Item','Execution records','Plan executions']);
  assert.equal(tabs[2].attributes['aria-pressed'],'true');
});

test('scheduled pseudo issues are labelled in the issue board',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  const row=panel.renderIssueRow(team,{id:'schedule-t1',title:'每日巡检',scheduled:true,status:'in_progress',progress:{known:true,total:1,completed:0},runs:[]});
  assert.match(text(row),/Scheduled item/);
});

test('task source chat opens the execution session with its originating Run',()=>{
  const opened=[];
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({}),openChat:(id,source)=>opened.push({id,source})});
  screen.team=team;screen.teamId='t1';screen.runId='r1';
  screen.detail={id:'r1',status:'completed',tasks:[],threads:[{id:'thread-1',providerSessionId:'execution-1'}]};
  const task={id:'task-1',memberId:'kimi',status:'completed',executionThreadId:'thread-1'};
  const card=screen.renderTask(task);
  const button=find(card,'task-actions')[0].children.find(n=>n.textContent==='Open source chat');
  assert.ok(button);button.onclick();
  assert.deepEqual(opened,[{id:'execution-1',source:{teamId:'t1',runId:'r1',taskId:'task-1'}}]);
});

test('self review is visibly labelled as non-independent',()=>{
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.team=team;screen.detail={id:'r1',status:'completed',tasks:[],workspace:{}};
  const card=screen.renderTask({id:'review',memberId:'buddy',status:'completed',reviewIndependence:false});
  assert.match(text(card),/Non-independent review/);
  const independent=screen.renderTask({id:'review-independent',memberId:'buddy',status:'completed',reviewIndependence:true});
  assert.doesNotMatch(text(independent),/Non-independent review/);
});

test('tasks group by phase and legacy tasks fall back to other',()=>{
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.team=team;screen.detail={id:'r1',status:'completed',tasks:[],workspace:{}};
  const box=screen.renderTaskGroups([
    {id:'design',memberId:'kimi',phase:'plan',status:'completed'},
    {id:'impl',memberId:'buddy',phase:'implement',status:'running'},
    {id:'legacy',memberId:'buddy',status:'completed'},
  ]);
  const groups=find(box,'team-phase');
  assert.equal(groups.length,3);
  assert.deepEqual(groups.map(group=>group.dataset.phase),['plan','implement','Other']);
  assert.match(text(groups[0]),/plan · 1\/1/);
  assert.match(text(groups[1]),/implement · 0\/1/);
  assert.match(text(groups[2]),/Other · 1\/1/);
  assert.ok(groups.every(group=>group.open));
});

test('teams panel localizes member roles and shows the readable project name',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  const card=panel.renderTeam(team);
  assert.deepEqual(find(card,'team-member').map(text),['★ Lead · Kimi','Member · CodeBuddy']);
  assert.equal(text(find(card,'team-card-meta')[0]),'2 members · Project mobile-bridge');
  // 没有 cwd 时才回落内部 key。
  const bare=panel.renderTeam({...team,projectCwd:''});
  assert.match(text(find(bare,'team-card-meta')[0]),/Project codex:7015435b2c37/);
});

test('dashboard groups actionable, active and ended runs and keeps unknown states actionable',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  panel.teams=[team];panel.view='records';
  const run=(id,status,updatedAt)=>({id,teamId:team.id,goal:id,status,updatedAt,
    taskCounts:{completed:1},currentOwner:{name:'Kimi'},nextAction:{label:'Review result'}});
  panel.runs.set(team.id,{rows:[
    run('ended','completed','2026-11-09T10:00:00Z'),
    run('active','collecting','2026-11-09T11:00:00Z'),
    run('unknown','future_status','2026-11-09T12:00:00Z'),
    run('paused','paused','2026-11-09T09:00:00Z'),
  ]});
  const activity=panel.renderActivity();
  assert.deepEqual(find(activity,'team-activity-group-title').map(text),['Needs attention2','Running1','Recently ended1']);
  assert.deepEqual(find(activity,'team-activity-row').map(row=>row.dataset.group),['attention','attention','active','terminal']);
  assert.equal(panel.runStats().attention,2);
  assert.equal(panel.teamLatestRun(team.id).id,'paused','the newest attention run outranks newer active runs');
  const unknownPanel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  unknownPanel.runs.set(team.id,{rows:[run('unknown','future_status','2026-11-09T12:00:00Z')]});
  assert.equal(find(unknownPanel.renderTeam(team),'team-card-run')[0].dataset.group,'attention');
});

test('team cards keep frequent actions visible and group management controls',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  const card=panel.renderTeam(team);
  assert.equal(find(card,'team-primary-actions')[0].children.length,2);
  assert.equal(find(card,'team-management')[0].children[0].textContent,'Team management');
  assert.equal(find(card,'team-management-actions')[0].children.length,5);
});

test('run echoes do not claim that a paused or completed run is still executing',()=>{
  const teamUI=require('../web/team.js');
  assert.match(teamUI.teamRunEchoLine({status:'collecting',currentOwner:{name:'Kimi'}}),/Kimi is working/);
  assert.match(teamUI.teamRunEchoLine({status:'paused',currentOwner:{name:'Kimi'}}),/Kimi · Lead/);
  assert.match(teamUI.teamRunEchoLine({status:'completed',currentOwner:{name:'Kimi'}}),/Kimi · Lead/);
});

test('graph nodes place agent and task status on the same meta row',()=>{
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.team=team;
  screen.detail={id:'r1',teamId:'t1',status:'completed',tasks:[]};
  const card=screen.renderGraphNode({id:'task-1',label:'任务',status:'completed',memberIds:['buddy'],agentKinds:['workbuddy']});
  assert.equal(find(find(card,'team-graph-node-head')[0],'team-badge').length,0);
  const meta=find(card,'team-graph-node-meta')[0];
  assert.equal(meta.children.length,2);
  assert.equal(text(meta.children[0]),'workbuddy');
  assert.equal(meta.children[1].className,'team-badge b-completed');
  assert.equal(meta.children[1].textContent,'Completed');
  const withoutAgent=screen.renderGraphNode({id:'task-2',label:'任务',status:'completed'});
  assert.equal(text(find(withoutAgent,'team-graph-node-meta')[0]),'Completed');
});

test('docked stop reasons and fixed task errors render in English',()=>{
  const reason='任务 lead-d1-0 停靠：账号认证失败或登录过期，任务已停靠（处理账号或配置后可继续）';
  const run={stopReason:reason,tasks:[{id:'lead-d1-0',status:'stopped',outcome:'auth'}]};
  assert.equal(teamRunStopReasonText(run),'Task lead-d1-0 docked: member sign-in failed (fix the account or configuration, then continue)');
  assert.equal(teamRunStopReasonText({stopReason:'用户取消运行'}),'Cancelled by user');
  // 列表行没有 tasks：按固定版式拆开翻译。
  assert.equal(teamRunStopReasonText({stopReason:reason}),'Task lead-d1-0 docked: account sign-in failed or expired; task parked (fix the account or configuration, then continue)');
  assert.equal(teamRunStopReasonText({stopReason:'未登记的原文'}),'未登记的原文');
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.team=team;
  screen.detail={id:'r1',teamId:'t1',status:'waiting_input',tasks:[]};
  const docked=text(screen.renderTask({id:'lead-d1-0',memberId:'buddy',status:'stopped',outcome:'auth',
    stopReason:'账号认证失败或登录过期，任务已停靠',error:'WorkBuddy 登录已失效，请在 CLI 中执行 /login 后重试',prompt:'检查'}));
  assert.match(docked,/Docked: member sign-in failed/);
  assert.match(docked,/WorkBuddy sign-in expired/);
  assert.doesNotMatch(docked,/账号认证失败|Stop reason/);
  const failed=text(screen.renderTask({id:'lead-d1-1',memberId:'buddy',status:'failed',error:'队列派发失败',prompt:'检查'}));
  assert.match(failed,/Queue dispatch failed/);
});

test('every wrapped Chinese literal in the web UI has an English entry',()=>{
  const missing=[];
  const files=fs.readdirSync(path.join(__dirname,'..','web')).filter(name=>name.endsWith('.js')&&name!=='i18n.js');
  for(const file of files){
    const source=fs.readFileSync(path.join(__dirname,'..','web',file),'utf8');
    // toast() 内部会 t()：它的字面量参数同样需要词条。
    for(const match of source.matchAll(/toast\(([^;]*?)\)(?=;|\)|,|\})/g)){
      for(const quoted of match[1].matchAll(/'([^']*[一-鿿][^']*)'/g))if(!(quoted[1] in BridgeI18n.dictionary))missing.push(file+': '+quoted[1]);
    }
    for(const match of source.matchAll(/\b(?:t|t_safe|BridgeI18n\.t)\('((?:[^'\\]|\\.)*)'\)/g)){
      const literal=match[1];
      if(/[一-鿿：、（）]/.test(literal)&&!(literal in BridgeI18n.dictionary))missing.push(file+': '+literal);
    }
  }
  const html=fs.readFileSync(path.join(__dirname,'..','web','index.html'),'utf8');
  for(const match of html.matchAll(/data-i18n(?:-[a-z-]+)?="([^"]+)"/g)){
    if(/[一-鿿]/.test(match[1])&&!(match[1] in BridgeI18n.dictionary))missing.push('index.html: '+match[1]);
  }
  assert.deepEqual([...new Set(missing)],[]);
});

test('resume hint follows the actual stop cause',()=>{
  const hint=detail=>Object.assign(Object.create(TeamRunScreen.prototype),{detail}).resumeHint();
  // d157 形态：认证停靠 + 一个失败分支，不能再显示「结果未知」。
  const docked=hint({stopReason:'任务 lead-d1-0 停靠：…',tasks:[{status:'stopped',outcome:'auth'},{status:'failed'}]});
  assert.match(docked,/resends the parked tasks/);
  assert.match(docked,/Failed tasks are not retried/);
  assert.doesNotMatch(docked,/Outcome-unknown/);
  assert.match(hint({stopReason:'',tasks:[{status:'outcome_unknown'}]}),/Outcome-unknown tasks are treated as verified/);
  assert.match(hint({stopReason:'budget_exhausted（…）',tasks:[]}),/budget/i);
  assert.match(hint({stopReason:'verify_failed（退出码 1）',tasks:[]}),/reruns the verify command/);
  assert.equal(hint({stopReason:'',tasks:[{status:'planned'}]}),'Continue dispatches the remaining runnable tasks.');
});

test('run page shows delivery evidence without accept or discard',()=>{
  global.t_safe=value=>BridgeI18n.t(value);
  const chat=require('../web/team-chat.js');
  global.renderTeamDeliveryEvidence=chat.renderTeamDeliveryEvidence;
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>{throw new Error('不应发请求');}});
  screen.teamId='t1';screen.runId='r1';screen.team=team;
  screen.detail={id:'r1',teamId:'t1',status:'completed',version:2,tasks:[],
    delivery:{agent:{completed:1,total:1},verify:{configured:false},pendingQuestions:0}};
  const box=screen.renderDelivery();
  assert.match(text(box),/Agent tasks: 1\/1 finished normally/);
  assert.doesNotMatch(text(box),/Accept|Discard|review the result/);
  assert.equal(find(box,'team-delivery-actions').length,0);
  assert.equal(typeof screen.setDelivery,'undefined');
  screen.detail.delivery={agent:{completed:1,total:1},review:{verdict:'passed',reviewIndependence:false},verify:{configured:true,status:'unknown'},pendingQuestions:0};
  const warnings=screen.renderDelivery();
  assert.match(text(warnings),/Review: Passed · Non-independent review/);
  assert.match(text(warnings),/Verification: Unknown outcome/);
});

test('run page shows a labeled back button that leaves the run',()=>{
  let left=false;
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({}),onNavigateAway:()=>{left=true;}});
  screen.team=team;
  screen.detail={id:'r1',teamId:'t1',status:'completed',tasks:[]};
  screen.render();
  const back=find(screen.root,'team-run-back')[0];
  assert.equal(back.textContent,'‹ Back');
  assert.equal(back.attributes['aria-label'],'Back');
  back.onclick();
  assert.equal(left,true);
});

test('dagre lays out task chains in reading direction with real dependency edges',()=>{
  const nodes=[
    {id:'lead',dependsOn:[]},
    {id:'review',dependsOn:['lead']},
    {id:'summary',dependsOn:['review']},
  ];
  const horizontal=teamGraphLayout(nodes,'LR');
  assert.ok(horizontal.nodes.lead.x<horizontal.nodes.review.x);
  assert.ok(horizontal.nodes.review.x<horizontal.nodes.summary.x);
  assert.equal(horizontal.nodes.lead.y,horizontal.nodes.summary.y);
  assert.deepEqual(horizontal.edges.map(edge=>[edge.from,edge.to]),[['lead','review'],['review','summary']]);
  assert.ok(horizontal.edges.every(edge=>edge.points.length>=2));
  const vertical=teamGraphLayout(nodes,'TB');
  assert.ok(vertical.nodes.lead.y<vertical.nodes.review.y);
  assert.ok(vertical.nodes.review.y<vertical.nodes.summary.y);
});

test('graph renders positioned DOM nodes and SVG edges without duplicate dependency captions',()=>{
  global.teamWorkflowTaskTitle=task=>task.prompt;
  global.teamWorkflowMemberLabel=(member,task)=>member?member.name+' · '+member.agentKind:task.memberId;
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.team=team;screen.detail={id:'r1',teamId:'t1',status:'running',tasks:[
    {id:'lead',memberId:'kimi',status:'completed',prompt:'理解目标'},
    {id:'review',memberId:'buddy',status:'running',prompt:'核对首页'},
    {id:'summary',memberId:'kimi',status:'planned',prompt:'汇总结果'},
  ]};
  screen.graph={loading:false,data:{nodes:[
    {id:'lead',label:'lead',status:'completed',memberIds:['kimi'],agentKinds:['kimi'],dependsOn:[]},
    {id:'review',label:'lead-d1-0',status:'running',memberIds:['buddy'],agentKinds:['workbuddy'],dependsOn:['lead']},
    {id:'summary',label:'lead-d1-1',status:'planned',memberIds:['kimi'],agentKinds:['kimi'],dependsOn:['lead','review']},
  ]}};
  const box=screen.renderGraph();
  const cards=find(box,'positioned');
  assert.equal(cards.length,3);
  assert.equal(find(box,'team-graph-edge').length,3);
  assert.equal(find(box,'team-graph-deps').length,0);
  assert.ok(cards.every(card=>card.style.left&&card.style.top&&card.style.height));
  assert.equal(cards[1].dataset.status,'running');
  assert.match(text(cards[0]),/理解目标/);
  assert.match(text(cards[1]),/核对首页/);
  assert.doesNotMatch(text(cards[1]),/lead-d1-0/);
  assert.match(text(cards[1]),/workbuddy · workbuddy/);
  assert.equal(find(find(box,'team-graph-stage')[0],'team-graph-edges').length,1,'SVG 与节点必须共享居中坐标容器');
  global.matchMedia=()=>({matches:true});
  const mobile=screen.renderGraph();
  assert.equal(find(mobile,'positioned').length,0,'手机端不使用超宽绝对定位画布');
  assert.ok(find(mobile,'team-graph-layer').length>0,'手机端回退为全宽分层卡片');
  delete global.matchMedia;
});

test('run screen places the execution graph before secondary statistics',()=>{
  const root=new FakeNode('div'),screen=new TeamRunScreen({root,request:async()=>({})});
  screen.detail={id:'r1',status:'running',tasks:[]};screen.stats={loading:false};
  screen.renderGraph=()=>screen.node('section','阶段图','team-graph');
  screen.renderStats=()=>screen.node('section','统计','team-stats');
  screen.render();
  const classes=find(root,'team-run-body')[0].children.map(node=>node.className);
  assert.ok(classes.indexOf('team-graph')<classes.indexOf('team-stats'));
});

test('vertical graph with skip dependencies fits the phone content width',()=>{
  const nodes=[
    {id:'lead',dependsOn:[]},
    {id:'lead-d1-0',dependsOn:['lead']},
    {id:'lead-d1-1',dependsOn:['lead','lead-d1-0']},
    {id:'lead-d1-1-s1-0',dependsOn:['lead','lead-d1-0','lead-d1-1']},
  ];
  const layout=teamGraphLayout(nodes,'TB',node=>node.id==='lead-d1-0'||node.id==='lead-d1-1-s1-0'?118:76);
  assert.ok(layout.width<=320,`纵向图宽度 ${layout.width} 超过手机内容区`);
  assert.ok(Object.values(layout.nodes).every(node=>node.width===180));
});

test('task_stopped projects cancelled without waiting for a detail refetch',()=>{
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.detail={tasks:[{id:'a',status:'running'}]};
  screen.applyEvent({type:'task_stopped',payload:{taskId:'a',status:'cancelled',stopReason:'用户取消运行'}});
  assert.equal(screen.detail.tasks[0].status,'cancelled');
  screen.applyEvent({type:'task_stopped',payload:{taskId:'a',stopReason:'依赖分支已终止，任务不再执行'}});
  assert.equal(screen.detail.tasks[0].status,'stopped');
  screen.applyEvent({type:'task_stopped',payload:{taskId:'a',stopReason:'superseded_by_first_success'}});
  assert.equal(screen.detail.tasks[0].status,'cancelled');
});

test('the reducing run state describes finalization separately from check results',()=>{
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  assert.equal(screen.runBadge('reducing').textContent,'Finalizing');
});

test('closing a run invalidates its pending initial read',async()=>{
  let resolveDetail;
  const detail=new Promise(resolve=>{resolveDetail=resolve;});
  const root=new FakeNode('div');
  const screen=new TeamRunScreen({root,request:url=>url.endsWith('/r1')?detail:Promise.resolve(team)});
  const opening=screen.openRun('t1','r1');
  screen.close();
  resolveDetail({id:'r1',tasks:[]});
  await opening;
  assert.equal(screen.detail,null);
  assert.equal(screen.stopFn,null);
});

test('an older detail response cannot overwrite a newer event',async()=>{
  let resolveDetail;
  const detail=new Promise(resolve=>{resolveDetail=resolve;});
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:()=>detail});
  screen.teamId='t1';screen.runId='r1';
  screen.detail={tasks:[{id:'a',status:'running'}]};
  const reading=screen.refetch();
  screen.lastSeq=2;
  screen.applyEvent({type:'task_completed',payload:{taskId:'a'}});
  resolveDetail({tasks:[{id:'a',status:'running'}]});
  await reading;
  clearTimeout(screen.refetchTimer);
  assert.equal(screen.detail.tasks[0].status,'completed');
});

test('an older detail response cannot overwrite a meta-only run transition',async()=>{
  let resolveDetail;
  const detail=new Promise(resolve=>{resolveDetail=resolve;});
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:()=>detail});
  screen.teamId='t1';screen.runId='r1';
  screen.detail={status:'dispatching',tasks:[]};
  const reading=screen.refetch();
  screen._lastMetaJSON='{"status":"waiting_input"}';
  screen.detail.status='waiting_input';
  resolveDetail({status:'dispatching',tasks:[]});
  await reading;
  clearTimeout(screen.refetchTimer);
  assert.equal(screen.detail.status,'waiting_input');
});

test('run control actions are disabled in flight and confirm success',async()=>{
  let resolveAction;
  const action=new Promise(resolve=>{resolveAction=resolve;});
  const posts=[];
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async(url,options={})=>{
    if(options.method==='POST'){posts.push(url);return action;}
    throw Object.assign(Error('not found'),{status:404});
  }});
  screen.teamId='t1';screen.runId='r1';screen.team=team;
  screen.detail={id:'r1',teamId:'t1',status:'dispatching',version:1,tasks:[]};
  screen.render();
  assert.equal(screen.actionBusy,false);
  const running=screen.postAction('pause');
  assert.equal(screen.actionBusy,true);
  const buttons=find(screen.root,'team-run-actions')[0].children.filter(node=>node.tag==='button');
  assert.ok(buttons.length>=2);
  assert.equal(buttons.every(node=>node.disabled),true,'busy 时整排控制按钮必须禁用');
  assert.equal(posts.length,1);
  // 第二次点击在 busy 期间应被直接丢弃。
  await screen.postAction('pause');
  assert.equal(posts.length,1);
  resolveAction({...screen.detail,status:'paused'});
  await running;
  assert.equal(screen.actionBusy,false);
  assert.match(text(screen.root),/Run paused/);
});
