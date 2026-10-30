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
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(){}
  get childNodes(){return this.children;}
  get classList(){const node=this;return{add(name){node.className+=(node.className?' ':'')+name;},remove(){},toggle(){},contains(name){return node.className.split(/\s+/).includes(name);}};}
}
global.document={createElement:tag=>new FakeNode(tag),body:new FakeNode('body'),querySelectorAll:()=>[]};
const {TeamsPanel,TeamRunScreen,teamRunStopReasonText}=require('../web/team.js');
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

test('run page lets the user accept an older pending result',async()=>{
  global.t_safe=value=>BridgeI18n.t(value);
  const chat=require('../web/team-chat.js');
  global.renderTeamDeliveryEvidence=chat.renderTeamDeliveryEvidence;global.teamDeliveryLabel=chat.teamDeliveryLabel;
  const calls=[];
  const screen=new TeamRunScreen({root:new FakeNode('div'),request:async(path,options)=>{calls.push([path,options]);return{...screen.detail,deliveryStatus:'accepted',version:3};}});
  screen.teamId='t1';screen.runId='r1';screen.team=team;
  screen.detail={id:'r1',teamId:'t1',status:'completed',deliveryStatus:'pending_review',version:2,tasks:[],
    delivery:{agent:{completed:1,total:1},verify:{configured:false},pendingQuestions:0}};
  screen.render=()=>{};
  const box=screen.renderDelivery();
  assert.match(text(box),/Execution finished; review the result/);
  assert.match(text(box),/Agent: 1\/1 Completed/);
  await screen.setDelivery('accepted');
  assert.equal(calls[0][0],'/api/teams/t1/runs/r1/delivery');
  assert.deepEqual(calls[0][1],{method:'POST',body:{status:'accepted',version:2}});
  assert.equal(screen.detail.deliveryStatus,'accepted');
  assert.equal(find(screen.renderDelivery(),'team-delivery-actions').length,0,'已接受后不再显示验收按钮');
});
