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

test('goal assessment preserves drafts and retries the same operation without losing the original scope',async()=>{
  const {renderTeamGoalReview}=require('../web/team.js');
  const outcome={scope:{id:'issue-a',hash:'original-source'},evidenceHash:'evidence-1',status:'pending_confirmation',label:'结果待确认',reason:'待核对',canConfirm:true,
    contract:{revision:2,deliverableKind:'answer',verify:{requirement:'not_required',reason:'咨询',status:'not_configured'},review:{requirement:'not_required',reason:'人工核对',status:'not_configured'},criteria:[{id:'original',text:'原始用户问题',state:'unknown'}]}};
  const drafts=new Map(),sent=[];let updates=0;
  const options={runVersion:8,drafts,submit:async body=>{sent.push(body);if(sent.length===1)throw Error('Network failed');return{version:9};},updated:()=>updates++};
  const panel=renderTeamGoalReview(outcome,options);
  const state=[...drafts.values()][0];state.reason='已核对答案';state.states.original='met';
  const record=panel.querySelectorAll('button').find(n=>n.textContent==='Record assessment');
  await record.listeners.click();
  assert.equal(updates,0);assert.match(text(panel),/Network failed/);
  await record.listeners.click();
  assert.equal(updates,1);assert.equal(sent[0].id,sent[1].id,'请求结果未知时重试必须复用操作 ID');
  assert.equal(sent[1].scopeId,'issue-a');assert.equal(sent[1].scopeHash,'original-source');assert.deepEqual(sent[1].states,{original:'met'});
  renderTeamGoalReview(outcome,options);
  assert.equal([...drafts.values()][0].reason,'已核对答案','轮询重绘保留输入');
  renderTeamGoalReview({...outcome,evidenceHash:'evidence-2'},options);
  assert.equal([...drafts.values()][0].states.original,'unknown','结果变化后必须重新核对');
});
function find(node,cls){const rows=[];const visit=current=>{if(String(current.className).split(/\s+/).includes(cls))rows.push(current);for(const child of current.children)visit(child);};visit(node);return rows;}
function text(node){return node.textContent+node.children.map(text).join('');}

function feedbackOutcome(overrides={}){
  return{scope:{id:'run',hash:'scope'},evidenceHash:'evidence',executionStatus:'completed',status:'pending_confirmation',label:'结果待确认',reason:'查看结果',canConfirm:true,
    result:{text:'回答已经给出'},contract:{revision:1,deliverableKind:'result',verify:{requirement:'auto',status:'not_configured'},review:{requirement:'auto',status:'not_configured'},criteria:[{id:'original',text:'说明 Git 回撤机制',state:'unknown'}]},...overrides};
}

test('result confirmation requires no acceptance setup or written explanation',async()=>{
  const{renderTeamGoalReview}=require('../web/team.js'),sent=[],outcome=feedbackOutcome();
  const panel=renderTeamGoalReview(outcome,{runVersion:4,drafts:new Map(),submit:async body=>sent.push(body)});
  const settings=find(panel,'team-goal-contract')[0];assert.equal(settings.open,false,'高级条件默认收起');
  const confirm=panel.querySelectorAll('button').find(button=>button.textContent==='Confirm requirements are met');
  assert.equal(confirm.disabled,false);await confirm.listeners.click();
  assert.equal(sent.length,1);assert.equal(sent[0].action,'assess');assert.equal(sent[0].reason,'');
  assert.deepEqual(sent[0].states,{original:'met'});assert.equal(sent[0].contract,undefined);
  assert.ok(!text(panel).includes('Choose a deliverable type'));assert.ok(!text(panel).includes('Choose a check requirement'));
});

test('cancelled, failed and running executions show only their existing results',()=>{
  const{renderTeamGoalReview}=require('../web/team.js');
  for(const executionStatus of ['cancelled','failed','dispatching']){
    const panel=renderTeamGoalReview(feedbackOutcome({executionStatus}),{runVersion:1,drafts:new Map()});
    assert.equal(panel.querySelectorAll('select').length,0);assert.equal(panel.querySelectorAll('textarea').length,0);assert.equal(panel.querySelectorAll('button').length,0);
    assert.match(text(panel),/回答已经给出/,'保留已有结果');assert.ok(!text(panel).includes('Result needs confirmation'));
  }
  const empty=renderTeamGoalReview(feedbackOutcome({executionStatus:'cancelled',result:null}),{drafts:new Map()});assert.equal(empty.hidden,true);
});

test('an active issue can close tracking without a run cancellation action',async()=>{
  const{renderTeamGoalReview}=require('../web/team.js'),sent=[];
  const outcome=feedbackOutcome({executionStatus:'dispatching',scope:{id:'issue-a',hash:'scope'}});
  const drafts=new Map(),panel=renderTeamGoalReview(outcome,{runVersion:7,drafts,submit:async body=>sent.push(body)});
  const buttons=panel.querySelectorAll('button');
  assert.equal(buttons.find(button=>button.textContent==='Confirm requirements are met'),undefined);
  assert.equal(buttons.find(button=>button.textContent==='Save completion criteria'),undefined);
  const close=buttons.find(button=>button.textContent==='Close goal tracking');
  assert.ok(close);[...drafts.values()][0].reason='不再需要追踪';await close.listeners.click();
  assert.equal(sent.length,1);assert.equal(sent[0].action,'close');assert.equal(sent[0].scopeId,'issue-a');assert.equal(sent[0].reason,'不再需要追踪');
});

test('workspace changes preserve the confirmed outcome and show both versions',()=>{
  const{renderTeamGoalReview}=require('../web/team.js');
  const outcome=feedbackOutcome({status:'satisfied',workspaceChange:{confirmedHead:'1234567890abcdef',currentHead:'abcdef1234567890',reason:'工作区版本已变化'}});
  const panel=renderTeamGoalReview(outcome,{runVersion:4,drafts:new Map()});
  assert.match(text(panel),/The workspace changed; the earlier assessment is preserved/);
  assert.match(text(panel),/1234567890ab → abcdef123456/);
});

test('shared task output is labelled in the goal result',()=>{
  const{renderTeamGoalReview}=require('../web/team.js');
  const outcome=feedbackOutcome({result:{text:'共享汇总',includesShared:true}});
  const panel=renderTeamGoalReview(outcome,{runVersion:4,drafts:new Map()});
  assert.match(text(panel),/Includes output from shared tasks/);
});

test('problem feedback preserves the original scope and needs only a concrete note',async()=>{
  const{renderTeamGoalReview}=require('../web/team.js'),drafts=new Map(),sent=[];
  const panel=renderTeamGoalReview(feedbackOutcome(),{runVersion:7,drafts,submit:async body=>sent.push(body)});
  const feedback=panel.querySelectorAll('button').find(button=>button.textContent==='Report a problem');
  await feedback.listeners.click();assert.equal(sent.length,0);assert.match(text(panel),/Describe what still needs/);
  [...drafts.values()][0].reason='缺少未提交文件的回撤说明';await feedback.listeners.click();
  assert.equal(sent[0].scopeHash,'scope');assert.deepEqual(sent[0].states,{original:'unmet'});assert.equal(sent[0].reason,'缺少未提交文件的回撤说明');
});

test('missing required evidence disables confirmation while allowing feedback',async()=>{
  const{renderTeamGoalReview}=require('../web/team.js'),sent=[];
  const panel=renderTeamGoalReview(feedbackOutcome({canConfirm:false,status:'pending_verification'}),{drafts:new Map(),submit:async body=>sent.push(body)});
  const confirm=panel.querySelectorAll('button').find(button=>button.textContent==='Confirm requirements are met');
  assert.equal(confirm.disabled,true);await confirm.listeners.click();assert.equal(sent.length,0);
  assert.equal(panel.querySelectorAll('button').find(button=>button.textContent==='Report a problem').disabled,false);
  const satisfied=renderTeamGoalReview(feedbackOutcome({status:'satisfied'}),{drafts:new Map()});
  assert.equal(satisfied.querySelectorAll('button').find(button=>button.textContent==='Confirm requirements are met').hidden,true,'已确认后不重复要求确认');
});

const team={id:'t1',name:'团队',leadId:'kimi',project:'codex:7015435b2c37',projectCwd:'/Users/me/mobile-bridge/',policy:{},members:[
  {id:'kimi',name:'kimi',role:'lead',agentKind:'kimi'},
  {id:'buddy',name:'workbuddy',role:'member',agentKind:'workbuddy'},
]};

test('project board loads only its team runs and preserves filtering during refresh',async()=>{
  const root=new FakeNode('div'),calls=[],opened=[];
  const panel=new TeamsPanel({root,teamId:'t1',openRun:(...args)=>opened.push(args),request:async url=>{
    calls.push(url);
    return url==='/api/teams'?{teams:[team,{...team,id:'t2',name:'别的团队'}]}:{runs:[{id:'r1',goal:'本项目任务',status:'waiting_input',updatedAt:'2026-11-10T00:00:00Z',taskCounts:{completed:1}}]};
  }});
  await panel.refreshAll();
  assert.deepEqual(calls,['/api/teams','/api/teams/t1/runs']);
  assert.equal(find(root,'team-card').length,0,'项目看板不重复呈现团队卡片和跨团队管理');
  assert.ok(!text(root).includes('New team'));
  assert.ok(!text(root).includes('别的团队'));
  assert.equal(text(find(root,'team-kpi')[0]),'1Runs');
  find(root,'team-activity-row')[0].onclick();assert.deepEqual(opened,[['t1','r1']]);
  panel.filter='active';await panel.refreshAll();assert.equal(panel.filter,'active');
  assert.equal(find(root,'team-activity-row').length,0,'筛选不能混入其他状态或团队');
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
  panel.teams=[team];
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
