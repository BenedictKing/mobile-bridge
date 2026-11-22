'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const {ChatTimeline}=require('../web/timeline.js');

class FakeClassList{
  constructor(node){this.node=node;}
  toggle(name,on){const names=new Set(this.node.className.split(/\s+/).filter(Boolean));if(on)names.add(name);else names.delete(name);this.node.className=[...names].join(' ');}
  contains(name){return this.node.className.split(/\s+/).includes(name);}
}
class FakeNode{
  constructor(tag='div'){
    this.tagName=tag.toUpperCase();this.children=[];this.dataset={};this.className='';this.classList=new FakeClassList(this);
    this.attributes={};this.hidden=false;this.textContent='';this.scrollTop=0;this.scrollHeight=2000;this.clientHeight=600;
    this.rect={top:100,bottom:700};this.listeners=new Map();
  }
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=nodes;}
  querySelector(){return null;}
  querySelectorAll(){return [];}
  addEventListener(type,handler){this.listeners.set(type,handler);}
  removeEventListener(type){this.listeners.delete(type);}
  setAttribute(name,value){this.attributes[name]=String(value);}
  removeAttribute(name){delete this.attributes[name];}
  getBoundingClientRect(){return this.rect;}
}

function setup(options={}){
  const ids={};
  for(const id of ['timeline','messages','older','newer','user-jump','user-jump-list','user-jump-count'])ids[id]=new FakeNode(id==='user-jump'?'details':'div');
  ids['user-jump'].hidden=true;
  global.document={documentElement:new FakeNode(),getElementById:id=>ids[id],createElement:tag=>new FakeNode(tag)};
  global.ResizeObserver=class{observe(){}disconnect(){}};
  global.requestAnimationFrame=callback=>{callback();return 1;};
  const timeline=new ChatTimeline({url:action=>action,request:async()=>({}),renderMeta(){},renderText(){},status(){},...options});
  return{timeline,ids};
}

test('duplicate and stale delta frames cannot append text twice or regress a cursor',()=>{
  const{timeline}=setup();timeline.epoch='e';timeline.sequence=4;
  const row={key:'a',version:'v4',text:'once'};timeline.rows.set('a',{row,sequence:4});
  timeline.applyDelta({epoch:'e',sequence:4,rows:[{key:'a',prevVersion:'v3',version:'v4',append:'once'}]});
  timeline.applyDelta({epoch:'old',sequence:100,rows:[{key:'a',prevVersion:'v4',version:'v5',append:'wrong'}]});
  assert.equal(row.text,'once');assert.equal(timeline.sequence,4);timeline.dispose();
});

test('one mismatched delta keeps all rows and the cursor unchanged until recovery',async()=>{
  const{timeline}=setup();timeline.epoch='e';timeline.sequence=1;
  const row={key:'a',version:'v1',text:'before'};timeline.rows.set('a',{row,sequence:1});
  let recover;timeline.read=()=>new Promise(resolve=>{recover=resolve;});
  timeline.apply=page=>{timeline.sequence=page.sequence;};
  const done=timeline.applyDelta({epoch:'e',sequence:2,meta:{status:'active'},rows:[{key:'a',prevVersion:'v1',version:'v2',append:'after'},{key:'missing',prevVersion:'x',version:'y',append:'lost'}]});
  assert.equal(row.text,'before');assert.equal(timeline.sequence,1);
  recover({epoch:'e',sequence:2,rows:[],meta:{}});await done;
  assert.equal(timeline.sequence,2);timeline.dispose();
});

test('an old delta recovery cannot reset a newer timeline generation',async()=>{
  const{timeline}=setup();timeline.epoch='e';timeline.sequence=1;
  let recover,applied=0;timeline.read=()=>new Promise(resolve=>{recover=resolve;});timeline.apply=()=>{applied++;};
  const done=timeline.applyDelta({epoch:'e',sequence:2,rows:[{key:'missing',prevVersion:'x'}]});
  timeline.generation++;timeline.epoch='new';timeline.sequence=3;
  recover({epoch:'e',sequence:2,reset:true,rows:[],meta:{}});await done;
  assert.equal(applied,0);assert.equal(timeline.epoch,'new');assert.equal(timeline.sequence,3);timeline.dispose();
});

test('an old full frame cannot overwrite metadata or files',()=>{
  const{timeline}=setup();timeline.epoch='e';timeline.sequence=4;timeline.meta={status:'idle'};
  timeline.apply({epoch:'e',sequence:3,rows:[],files:[{id:'old'}],meta:{status:'active'}});
  assert.deepEqual(timeline.meta,{status:'idle'});assert.equal(timeline.files.size,0);timeline.dispose();
});

test('history already ahead of the live cursor cannot receive an older append',async()=>{
  const{timeline}=setup();timeline.epoch='e';timeline.sequence=1;
  const row={key:'a',version:'same-prefix',text:'newer'};timeline.rows.set('a',{row,sequence:5});
  let recovered=0;timeline.read=async()=>{recovered++;return{epoch:'e',sequence:5};};timeline.apply=page=>{timeline.sequence=page.sequence;};
  await timeline.applyDelta({epoch:'e',sequence:3,rows:[{key:'a',prevVersion:'same-prefix',version:'old',append:'old append'}]});
  assert.equal(row.text,'newer');assert.equal(timeline.sequence,5);assert.equal(recovered,1);timeline.dispose();
});

function addUser(timeline,ids,key,text,top){
  const row={key,role:'user',text,order:timeline.rows.size};
  const node=new FakeNode();node.dataset.key=key;node.rect={top,bottom:top+80};
  timeline.rows.set(key,{row,sequence:1});timeline.nodes.set(key,{node,stamp:''});ids.messages.children.push(node);
  return row;
}

test('user jump lists only user messages and marks the latest at bottom',()=>{
  const{timeline,ids}=setup();
  const first=addUser(timeline,ids,'u1','第一条\n需要换行的消息',120);
  const second=addUser(timeline,ids,'u2','第二条消息',500);
  timeline.renderUserJump([first,{key:'a1',role:'assistant',text:'回复',order:1.5},second],true);
  assert.equal(ids['user-jump'].hidden,false);
  assert.equal(ids['user-jump-count'].textContent,'2');
  assert.equal(ids['user-jump-list'].children.length,2);
  assert.equal(ids['user-jump-list'].children[0].children[1].textContent,'第一条 需要换行的消息');
  assert.equal(ids['user-jump-list'].children[1].attributes['aria-current'],'location');
  assert.equal(ids['user-jump-list'].scrollTop,ids['user-jump-list'].scrollHeight);
  timeline.dispose();
});

test('Run source navigation loads older history and targets its original turn',async()=>{
  const{timeline,ids}=setup();
  const newer=addUser(timeline,ids,'newer','其他任务',500);newer.turnId='other-turn';
  timeline.hasMore=true;
  let pages=0;
  timeline.loadOlder=async()=>{pages++;const target=addUser(timeline,ids,'original','本任务',300);target.turnId='source-turn';timeline.hasMore=false;};
  assert.equal(await timeline.jumpToTurn('source-turn'),true);
  assert.equal(pages,1);
  assert.equal(ids.timeline.scrollTop,188);
  assert.equal(timeline.following,false);
  timeline.dispose();
});

test('Run source turn navigation waits for cold history to become available',()=>{
  const{timeline,ids}=setup({initialTurnId:'source-turn'}),jumped=[];
  ids.messages.insertBefore=node=>ids.messages.children.push(node);
  timeline.fillRecent=()=>{};
  timeline.jumpToTurn=id=>jumped.push(id);
  const page={epoch:'e',sequence:1,rows:[],files:[],meta:{loadingHistory:true},hasMore:false};
  timeline.apply(page,true);
  assert.deepEqual(jumped,[],'空历史占位不能消耗任务定位请求');
  timeline.apply({...page,sequence:2,meta:{loadingHistory:false},rows:[{key:'original',role:'user',text:'本任务',turnId:'source-turn',order:0}]});
  assert.deepEqual(jumped,['source-turn']);
  timeline.apply({...page,sequence:3,meta:{loadingHistory:false}});
  assert.deepEqual(jumped,['source-turn'],'后续更新不能把用户拉回原位置');
  timeline.dispose();
});

test('Run source navigation loads the beginning of a partially visible turn',async()=>{
  const{timeline,ids}=setup(),jumped=[];
  const later=addUser(timeline,ids,'later','任务进行中的补充',500);
  Object.assign(later,{turnId:'source-turn',order:30});
  timeline.hasMore=true;
  timeline.jumpToUser=async key=>jumped.push(key);
  timeline.loadOlder=async()=>{
    const first=addUser(timeline,ids,'first','原始任务',300);
    Object.assign(first,{turnId:'source-turn',order:10});
    const prior=addUser(timeline,ids,'prior','上一回合',100);
    Object.assign(prior,{turnId:'prior-turn',order:0});
  };
  assert.equal(await timeline.jumpToTurn('source-turn'),true);
  assert.deepEqual(jumped,['first'],'不能把当前加载到的后续补充误当成原始任务');
  timeline.dispose();
});

test('team conversations hide gateway protocol rows without hiding ordinary user messages',()=>{
  const{timeline,ids}=setup();
  timeline.hideInternalTeamMessages=true;
  const internal=[
    '[Team review] review instructions',
    '[Team context] delegated task context',
    '[Team delegation] planner instructions',
    '[Team historical issue routing] issue instructions',
    '[Team account quotas] usage context',
  ].map((text,index)=>({key:'internal-'+index,role:'user',text,order:index}));
  const ordinary={key:'ordinary',role:'user',text:'请检查团队画布',order:1};
  assert.ok(internal.every(row=>timeline.rowVisible(row)===false));
  assert.equal(timeline.rowVisible(ordinary),true);
  timeline.userIndex=[...internal.map(({key,text})=>({key,text})),ordinary];
  timeline.renderUserJump([ordinary]);
  assert.equal(ids['user-jump-count'].textContent,'1');
  assert.equal(ids['user-jump-list'].children[0].dataset.key,'ordinary');
  timeline.dispose();
});

test('user jump click scrolls to the message and scroll tracking updates the active item',()=>{
  const{timeline,ids}=setup();
  const first=addUser(timeline,ids,'u1','第一条',120),second=addUser(timeline,ids,'u2','第二条',500);
  timeline.renderUserJump([first,second]);
  ids['user-jump-list'].children[1].onclick();
  assert.equal(ids.timeline.scrollTop,388);
  assert.equal(timeline.following,false);
  assert.equal(ids['user-jump-list'].children[1].classList.contains('active'),true);
  timeline.updateUserJumpActive();
  assert.equal(ids['user-jump-list'].children[0].attributes['aria-current'],'location');
  assert.equal(ids['user-jump-list'].children[1].attributes['aria-current'],undefined);
  timeline.renderUserJump([]);
  assert.equal(ids['user-jump'].hidden,true);
  timeline.dispose();
});

test('user jump floats at the top right on phones and large screens',()=>{
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.chat\{position:relative\}/);
  assert.match(css,/\.user-jump\{position:absolute;z-index:4;top:76px;right:max\(16px,env\(safe-area-inset-right\)\)/);
  assert.match(css,/\.user-jump\{[^}]*margin:0/);
  assert.match(css,/\.composer\{position:relative;padding:6px 0 max\(8px,env\(safe-area-inset-bottom\)\)/);
  assert.match(css,/\.composer-options\.is-open\{display:grid\}/);
  assert.match(css,/\.user-jump-list\{max-height:min\(30dvh,280px\)/);
  assert.match(css,/@media\(min-width:721px\)\{\s*\.user-jump\{position:absolute/);
  assert.doesNotMatch(css,/\.user-jump\{display:none\}/);
});

test('composer tools and work modes use separate, labeled controls',()=>{
  const html=readFileSync(require.resolve('../web/index.html'),'utf8');
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  const tools=html.match(/<button id="composer-options-button"[\s\S]*?<\/button>/)[0];
  const mode=html.match(/<div id="work-mode-button"[\s\S]*?<\/div>\s*<\/div>/)[0];
  assert.match(tools,/<rect x="4" y="4"/);
  assert.match(mode,/role="group" aria-label="工作模式"/);
  for(const workMode of ['default','plan','goal'])assert.match(mode,new RegExp('data-work-mode="'+workMode+'"'));
  assert.match(mode,/class="mode-label-full" data-i18n="普通模式">普通模式/);
  assert.match(mode,/class="mode-label-short" data-i18n="普通">普通/);
  assert.match(mode,/class="mode-label-full" data-i18n="计划模式">计划模式/);
  assert.match(mode,/class="mode-label-short" data-i18n="计划">计划/);
  assert.match(mode,/class="mode-label-full" data-i18n="目标模式">目标模式/);
  assert.match(mode,/class="mode-label-short" data-i18n="目标">目标/);
  assert.doesNotMatch(mode,/\stitle=/);
  assert.doesNotMatch(mode,/<svg/);
  assert.doesNotMatch(html,/id="mode-hint"/);
  assert.match(css,/\.mode-label-short\{display:none\}/);
  assert.match(css,/@media\(max-width:720px\)\{[\s\S]*?\.mode-label-full\{display:none\}[\s\S]*?\.mode-label-short\{display:inline\}/);
  assert.doesNotMatch(css,/mode-hint/);
});

test('user jump lists prompts from the meta index beyond the loaded window',()=>{
  const{timeline,ids}=setup();
  timeline.userIndex=[{key:'u-old',text:'很早的一条消息'},{key:'u1',text:'最新消息'}];
  const row=addUser(timeline,ids,'u1','最新消息',120);
  timeline.renderUserJump([row]);
  assert.equal(ids['user-jump'].hidden,false);
  assert.equal(ids['user-jump-count'].textContent,'2');
  assert.equal(ids['user-jump-list'].children.length,2);
  assert.equal(ids['user-jump-list'].children[0].children[1].textContent,'很早的一条消息');
  timeline.dispose();
});

test('jumping to an unloaded prompt pages history until the node exists',async()=>{
  const{timeline,ids}=setup();
  const newest=addUser(timeline,ids,'u1','最新消息',120);
  timeline.epoch='e1';timeline.sequence=1;timeline.hasMore=true;timeline.before=null;
  timeline.userIndex=[{key:'u-old',text:'很早的一条消息'},newest];
  timeline.renderUserJump([newest]);
  timeline.read=async()=>({epoch:'e1',sequence:2,before:'b0',hasMore:false,meta:{},
    rows:[{key:'u-old',role:'user',text:'很早的一条消息',order:0}]});
  const originalRender=timeline.renderRow.bind(timeline);
  timeline.renderRow=(row,open)=>{const node=originalRender(row,open);if(row.key==='u-old')node.rect={top:300,bottom:380};return node;};
  await timeline.jumpToUser('u-old');
  assert.ok(timeline.nodes.has('u-old'),'paged row not loaded');
  assert.equal(ids.timeline.scrollTop,188,'viewport did not scroll to the row');
  assert.equal(timeline.userJumpButtons.get('u-old').classList.contains('active'),true);
  timeline.dispose();
});

test('ACP latest user edits use a branch while Codex keeps in-place edits',()=>{
  for(const testCase of [
    {agent:'kimi',label:'编辑并新建分支',action:'edit-fork'},
    {agent:'zcode',label:'编辑并新建分支',action:'edit-fork'},
    {agent:'codex',label:'编辑并重新发送',action:'edit'},
  ]){
    const{timeline}=setup();
    const node=new FakeNode(),bar=new FakeNode();
    node.querySelector=selector=>selector==='.message-actions'?bar:null;
    timeline.nodes.set('u1',{node,stamp:''});
    timeline.rows.set('u1',{row:{key:'u1',turnId:'turn-1',editable:true,turnStatus:'completed'},sequence:1});
    timeline.meta={agent:testCase.agent,latestUserTurnId:'turn-1',status:'idle'};
    let action='';timeline.onAction=value=>{action=value;};
    timeline.refreshActions();
    assert.equal(bar.children.length,1);
    assert.equal(bar.children[0].textContent,testCase.label);
    bar.children[0].onclick();
    assert.equal(action,testCase.action);
    timeline.dispose();
  }
});

test('scrolling away from the latest message shows a persistent jump-to-latest button',()=>{
  const{timeline,ids}=setup();
  assert.equal(ids.newer.hidden,true);
  assert.equal(ids.newer.textContent,'跳转到最后 ↓');
  ids.timeline.scrollTop=0;
  timeline.onScroll();
  assert.equal(timeline.following,false);
  assert.equal(ids.newer.hidden,false);
  assert.equal(ids.newer.textContent,'跳转到最后 ↓');
  ids.newer.textContent='有新内容 ↓';
  ids.timeline.scrollTop=20;
  timeline.onScroll();
  assert.equal(ids.newer.textContent,'有新内容 ↓','scrolling must not erase the new-content hint');
  ids.newer.onclick();
  assert.equal(ids.timeline.scrollTop,ids.timeline.scrollHeight);
  assert.equal(timeline.following,true);
  assert.equal(ids.newer.hidden,true);
  assert.equal(ids.newer.textContent,'跳转到最后 ↓');
  timeline.dispose();
});

test('steered event is a readable status without raw JSON',()=>{
  const{timeline}=setup();
  const node=timeline.renderRow({role:'activity',kind:'steered',title:'steered',text:'{"type":"steered","id":"internal"}'},false);
  assert.equal(node.tagName,'DIV');
  assert.equal(node.children.length,1);
  assert.equal(node.children[0].textContent,'已向进行中的对话补充指示');
  timeline.dispose();
});

test('gateway migration and compaction stay visible as source-chat status records',()=>{
  for(const kind of ['gatewayMigration','gatewayCompaction']){
    const{timeline}=setup();
    const text=kind==='gatewayMigration'?'会话迁移已完成：后续请在新会话继续。':'网关压缩已完成：后续请在新会话继续。';
    const row={key:kind,role:'activity',kind,title:kind,status:'completed',text};
    const node=timeline.renderRow(row,false);
    assert.equal(node.tagName,'DIV');
    assert.match(node.className,/status-event/);
    assert.equal(node.children.length,1);
    assert.equal(node.children[0].textContent,text);
    timeline.showProcess=false;
    assert.equal(timeline.rowVisible(row),true,'maintenance records do not depend on tool activity visibility');
    timeline.dispose();
  }
});

test('sleep rows show readable durations without exposing raw JSON',async()=>{
  const{timeline}=setup();
  const full=JSON.stringify({type:'sleep',id:'call-internal',durationMs:45000});
  const node=timeline.renderRow({key:'sleep',role:'activity',kind:'sleep',title:'sleep',text:full},false);
  assert.equal(node.dataset.tool,'');
  assert.equal(node.children[0].textContent,'等待');
  assert.equal(node.children[1].textContent,'等待 45 秒');
  assert.doesNotMatch(node.children[1].textContent,/call-internal|durationMs|\{/);

  const row={key:'legacy-sleep',version:'v1',role:'activity',kind:'sleep',title:'sleep',truncated:true,text:full.slice(0,40)};
  timeline.rows.set(row.key,{row,sequence:1});
  const legacy=timeline.renderRow(row,false);legacy.isConnected=true;
  assert.equal(legacy.children[1].textContent,'等待时长未知');
  assert.doesNotMatch(legacy.children[1].textContent,/\{/);
  timeline.read=async()=>({version:'v1',text:full,offset:0,next:null});
  await legacy.children[2].onclick();
  assert.equal(legacy.children[1].textContent,'等待 45 秒');
  timeline.dispose();
});

test('command rows are collapsed to an accessible status and expand to full text',()=>{
  const cases=[
    [{kind:'commandExecution',status:'inProgress',text:'go test ./...'},'执行命令（进行中）','inProgress'],
    [{kind:'commandExecution',status:'completed',exitCode:0,text:'go test ./...\nPASS'},'执行命令','completed'],
    [{kind:'commandExecution',status:'completed',exitCode:2,text:'a very long command\nmultiple\noutput lines'},'执行命令','failed'],
    [{kind:'CommandExecution',status:'cancelled',text:'stop this'},'执行命令（已取消）','cancelled'],
    [{kind:'commandExecution',status:'completed',text:'missing exit code'},'执行命令（结果未知）','unknown'],
    [{kind:'commandExecution',status:'outcome_unknown',exitCode:0,text:'uncertain'},'执行命令（结果未知）','unknown'],
    [{kind:'toolCall',toolKind:'execute',status:'completed',text:'git diff --check'},'执行命令','completed'],
    [{kind:'toolCall',toolKind:'execute',status:'failed',text:'make check'},'执行命令','failed'],
  ];
  for(const [fixture,label,state] of cases){
    const{timeline}=setup();
    const row={key:'command-1',role:'activity',version:1,truncated:true,...fixture};
    const node=timeline.renderRow(row,false);
    assert.equal(node.tagName,'DETAILS');
    assert.equal(node.open,false,'new commands must start collapsed');
    assert.equal(node.dataset.tool,'');
    assert.equal(node.dataset.command,'');
    assert.equal(node.dataset.commandStatus,state);
    assert.equal(node.children[0].tagName,'SUMMARY');
    assert.equal(node.children[0].textContent,label);
    if(state==='completed'||state==='failed'){
      const icon=node.children[0].children.at(-1);
      assert.equal(icon.className,'diff-exit '+(state==='failed'?'fail':'ok'));
      assert.equal(icon.textContent,state==='failed'?'✗':'✓');
    }
    assert.ok(!node.children[0].textContent.includes(row.text),'summary must not expose command or output');
    assert.equal(node.children[1].textContent,row.text,'expanded body keeps command and output');
    assert.equal(node.children[1].hidden,false);
    assert.equal(node.children[2].hidden,false,'truncated rows keep continuation control');
    const opened=timeline.renderRow(row,true);
    assert.equal(opened.open,true,'redraw preserves an open command');
    timeline.dispose();
  }
});

test('command CSS hides all body preview and keeps non-command activities unchanged',()=>{
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.activity\[data-command\]:not\(\[open\]\) \.activity-body/);
  assert.match(css,/\.activity summary:focus-visible/);
  const{timeline}=setup();
  const thought=timeline.renderRow({key:'thought',role:'activity',kind:'reasoning',title:'思考摘要',text:'摘要'},false);
  assert.equal(thought.dataset.command,undefined);
  assert.equal(thought.dataset.tool,undefined);
  timeline.dispose();
});

test('reasoning uses the Markdown renderer while command output stays literal',()=>{
  const calls=[];
  const{timeline}=setup({renderText:(body,text)=>calls.push({body,text})});
  const row={key:'md-thought',role:'activity',kind:'reasoning',text:'## Plan\n\n- **Check** `code`'};
  timeline.renderRow(row,false);
  assert.equal(calls.length,1);
  assert.equal(calls[0].body.tagName,'DIV');
  assert.equal(calls[0].text,row.text);
  timeline.renderRow({key:'cmd',role:'activity',kind:'commandExecution',text:'**literal**'},false);
  assert.equal(calls.length,1);
  timeline.dispose();
});

test('automatic approval reviews have readable labels and keep untrusted content literal',()=>{
  const{timeline}=setup();
  const node=timeline.renderRow({key:'approval',role:'activity',kind:'automaticApprovalReview',title:'automaticApprovalReview',
    text:JSON.stringify({id:'internal',type:'automaticApprovalReview',status:'completed',review:{decision:'allow',reason:'<img src=x onerror=alert(1)>'}})},false);
  assert.equal(node.children[0].textContent,'自动审批审查');
  const body=node.children[1];
  assert.match(body.textContent,/审批决定：已批准/);
  assert.match(body.textContent,/原因：<img src=x onerror=alert\(1\)>/);
  assert.doesNotMatch(body.textContent,/automaticApprovalReview|internal/);
  assert.equal(body.children.length,0,'审查内容不能当作 HTML 执行');
  timeline.dispose();
});

test('ACP agentThought uses Markdown and obeys the thinking visibility setting',()=>{
  const painted=[];
  const{timeline}=setup({renderText:(body,text)=>painted.push({body,text})});
  const row={key:'acp-thought',role:'activity',kind:'agentThought',title:'思考摘要',text:'### 检查\n- **条件** `code`'};
  const node=timeline.renderRow(row,false);
  assert.equal(painted.length,1);assert.equal(painted[0].body.tagName,'DIV');
  assert.equal(painted[0].text,row.text);assert.match(painted[0].body.className,/reasoning-body/);
  timeline.showProcess=true;timeline.showReasoning=false;
  assert.equal(timeline.rowVisible(row),false,'思考开关应覆盖 ACP 的 agentThought');
  timeline.showReasoning=true;assert.equal(timeline.rowVisible(row),true);
  assert.equal(node.children[0].textContent,'思考摘要');
  timeline.dispose();
});

test('truncated legacy approval JSON stays readable and expands to full rationale',async()=>{
  const{timeline}=setup();
  const full=JSON.stringify({type:'automaticApprovalReview',id:'automatic-approval-review:'+('i'.repeat(150)),targetItemId:'internal-target',startedAtMs:123,
    status:'approved',riskLevel:'low',rationale:'已核对目标和范围。',action:{type:'command',source:'unifiedExec',command:'kill -INT 123',cwd:'/workspace'}});
  const row={key:'review-preview',version:'v1',role:'activity',kind:'automaticApprovalReview',title:'automaticApprovalReview',status:'approved',truncated:true,text:full.slice(0,180)};
  timeline.rows.set(row.key,{row,sequence:1});
  const node=timeline.renderRow(row,false);node.isConnected=true;
  assert.equal(node.children[0].textContent,'自动审批审查 · 已批准');
  const body=node.children[1];
  assert.match(body.textContent,/审批状态：已批准/);assert.match(body.textContent,/展开查看审查依据/);
  assert.doesNotMatch(body.textContent,/automaticApprovalReview|internal-target|\{/);
  timeline.read=async()=>({version:'v1',text:full,offset:0,next:null});
  await node.children[2].onclick();
  assert.match(body.textContent,/审查依据：已核对目标和范围。/);
  assert.match(body.textContent,/命令：kill -INT 123/);
  assert.doesNotMatch(body.textContent,/startedAtMs|internal-target|unifiedExec/);
  timeline.dispose();
});

test('approved reviews hide their collapsed bodies without pretending the command succeeded',()=>{
  const{timeline}=setup();
  const node=timeline.renderRow({key:'normalized',role:'activity',kind:'automaticApprovalReview',status:'approved',title:'自动审批审查',
    text:'审批状态：已批准\n风险等级：低\n审查依据：检查已授权。',truncated:true},false);
  assert.equal(node.children[0].textContent,'自动审批审查 · 已批准');
  assert.equal(node.children[0].children.length,0,'审批通过不能当作执行命令成功');
  assert.equal(node.dataset.approvalStatus,'approved');
  assert.match(node.children[1].textContent,/风险等级：低/);
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.activity\[data-approval-status=approved\]:not\(\[open\]\) \.activity-body\{display:none\}/);
  assert.match(css,/:not\(\[data-approval-status=approved\]\)/,'三行预览规则必须排除已批准审查');
  timeline.dispose();
});

test('denied and pending reviews retain their previews and legacy approved decisions collapse',()=>{
  const{timeline}=setup();
  for(const status of ['denied','pending','inProgress','failed']){
    const node=timeline.renderRow({key:status,role:'activity',kind:'automaticApprovalReview',status,text:'审查依据：需要确认。'},false);
    assert.equal(node.dataset.approvalStatus,'');
    assert.match(node.children[1].textContent,/需要确认/);
  }
  const node=timeline.renderRow({key:'legacy-allow',role:'activity',kind:'automaticApprovalReview',
    text:JSON.stringify({status:'completed',review:{decision:'allow',reason:'已确认。'}})},true);
  assert.equal(node.dataset.approvalStatus,'approved');
  assert.equal(node.open,true);
  assert.match(node.children[1].textContent,/原因：已确认。/);
  timeline.dispose();
});

test('context compaction renders the actual completion flag and trigger without raw fields',()=>{
  const{timeline}=setup();
  for(const [completed,turnStatus,status] of [[false,'inProgress','压缩中'],[true,'inProgress','已完成'],[false,'interrupted','已中断'],[false,'completed','已完成']]){
    const node=timeline.renderRow({key:'compaction',role:'activity',kind:'contextCompaction',turnStatus,
      text:JSON.stringify({type:'contextCompaction',id:'internal-id',startedAtMs:123,completed,source:'manual'})},false);
    assert.equal(node.children[0].textContent,'上下文压缩 · '+status);
    assert.match(node.children[1].textContent,/触发方式：手动/);
    assert.doesNotMatch(node.children[1].textContent,/contextCompaction|internal-id|startedAtMs|\{/);
  }
  timeline.dispose();
});

test('truncated legacy compaction stays conservative then updates on full detail',async()=>{
  const{timeline}=setup();
  const full=JSON.stringify({type:'contextCompaction',id:'i'.repeat(200),completed:true,source:'automatic'});
  const row={key:'legacy-compaction',version:'v1',role:'activity',kind:'contextCompaction',turnStatus:'inProgress',truncated:true,text:full.slice(0,180)};
  timeline.rows.set(row.key,{row,sequence:1});
  const node=timeline.renderRow(row,false);node.isConnected=true;
  assert.equal(node.children[0].textContent,'上下文压缩 · 状态待同步');
  assert.doesNotMatch(node.children[1].textContent,/contextCompaction|\{/);
  timeline.read=async()=>({version:'v1',text:full,offset:0,next:null});
  await node.children[2].onclick();
  assert.equal(node.children[0].textContent,'上下文压缩 · 已完成');
  assert.equal(node.children[1].textContent,'压缩状态：已完成\n触发方式：自动');
  timeline.nodes.set(row.key,{node});
  node.querySelector=selector=>selector===':scope>.who,:scope>summary'?node.children[0]:null;
  timeline.relabel();
  assert.equal(node.children[0].textContent,'上下文压缩 · 已完成','刷新标签时保留已加载详情的压缩状态');
  timeline.dispose();
});
