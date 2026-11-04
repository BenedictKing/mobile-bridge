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
  addEventListener(){}
  querySelector(){return null;}
  get childNodes(){return this.children;}
  get classList(){const node=this;return{add(name){node.className+=(node.className?' ':'')+name;},remove(){},toggle(){},contains(name){return node.className.split(/\s+/).includes(name);}};}
}
global.document={createElement:tag=>new FakeNode(tag),createElementNS:(namespace,tag)=>new FakeNode(tag),body:new FakeNode('body'),querySelectorAll:()=>[]};
global.dagre=require('../web/vendor/dagre.min.js');
const {TeamsPanel,TeamRunScreen,teamRunStopReasonText,teamGraphLayout}=require('../web/team.js');
function find(node,cls){const rows=[];const visit=current=>{if(String(current.className).split(/\s+/).includes(cls))rows.push(current);for(const child of current.children)visit(child);};visit(node);return rows;}
function text(node){return node.textContent+node.children.map(text).join('');}

const team={id:'t1',name:'团队',leadId:'kimi',project:'codex:7015435b2c37',projectCwd:'/Users/me/mobile-bridge/',policy:{},members:[
  {id:'kimi',name:'kimi',role:'lead',agentKind:'kimi'},
  {id:'buddy',name:'workbuddy',role:'member',agentKind:'workbuddy'},
]};

test('teams panel localizes member roles and shows the readable project name',()=>{
  const panel=new TeamsPanel({root:new FakeNode('div'),request:async()=>({})});
  const card=panel.renderTeam(team);
  assert.deepEqual(find(card,'team-member').map(text),['★ Lead · kimi · Kimi','Member · workbuddy · CodeBuddy']);
  assert.equal(text(find(card,'team-card-meta')[0]),'2 members · Project mobile-bridge');
  // 没有 cwd 时才回落内部 key。
  const bare=panel.renderTeam({...team,projectCwd:''});
  assert.match(text(find(bare,'team-card-meta')[0]),/Project codex:7015435b2c37/);
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
  assert.match(text(box),/Agent: 1\/1 Completed/);
  assert.doesNotMatch(text(box),/Accept|Discard|review the result/);
  assert.equal(find(box,'team-delivery-actions').length,0);
  assert.equal(typeof screen.setDelivery,'undefined');
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
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async()=>({})});
  screen.team=team;screen.detail={id:'r1',teamId:'t1',status:'running',tasks:[]};
  screen.graph={loading:false,data:{nodes:[
    {id:'lead',label:'lead',status:'completed',agentKinds:['kimi'],dependsOn:[]},
    {id:'review',label:'lead-d1-0',status:'running',agentKinds:['workbuddy'],dependsOn:['lead']},
    {id:'summary',label:'lead-d1-1',status:'planned',agentKinds:['kimi'],dependsOn:['lead','review']},
  ]}};
  const box=screen.renderGraph();
  const cards=find(box,'positioned');
  assert.equal(cards.length,3);
  assert.equal(find(box,'team-graph-edge').length,3);
  assert.equal(find(box,'team-graph-deps').length,0);
  assert.ok(cards.every(card=>card.style.left&&card.style.top&&card.style.height));
  assert.equal(cards[1].dataset.status,'running');
  assert.equal(find(find(box,'team-graph-stage')[0],'team-graph-edges').length,1,'SVG 与节点必须共享居中坐标容器');
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
