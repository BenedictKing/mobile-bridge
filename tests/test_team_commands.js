'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

// 从 app.js 抽出命令面板的纯函数段（与 test_queue.js 的抽取惯例一致）。
// defaultCompact 模拟设置里的默认压缩档（BridgePresentation.defaultCompact）。
function extract({team=false,defaultCompact}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('const COMPACT_PROMPTS=');
  const end=source.indexOf('function compactGateway',start);
  assert.ok(start>=0&&end>start,'composer command block not found in app.js');
  const context={t:value=>value,currentId:'chat',currentTeam:team,state:{id:'chat',agent:'kimi',agents:[{kind:'kimi'}]},window:{BridgePresentation:{defaultCompact:()=>defaultCompact}}};
  vm.runInNewContext(source.slice(start,end)+'\nthis.composerCommandRows=composerCommandRows;this.parseTeamCommand=parseTeamCommand;this.parseCompactCommand=parseCompactCommand;this.parsePlainCommand=parsePlainCommand;this.parseMigrateCommand=parseMigrateCommand;this.runMigrateCommand=runMigrateCommand;this.runGatewayCompact=runGatewayCompact;this.transcriptMaintenanceAvailable=transcriptMaintenanceAvailable;this.compactAvailable=compactAvailable;this.resolveDefaultCompact=resolveDefaultCompact;this.defaultCompactPreference=defaultCompactPreference;this.nativeCompactPrompt=nativeCompactPrompt;',context);
  return context;
}
const same=(actual,expected)=>assert.deepEqual(JSON.parse(JSON.stringify(actual)),expected);

test('team chat leaves maintenance and slash commands to TeamInput routing',()=>{
  const {parseTeamCommand}=extract({team:true});
  assert.equal(parseTeamCommand('/new'),null);
  assert.equal(parseTeamCommand('  /new \n'),null);
  assert.equal(parseTeamCommand('/compress'),null);
  assert.equal(parseTeamCommand('/compress main 继续实现'),null);
  assert.equal(parseTeamCommand('/migrate kimi'),null);
  assert.equal(parseTeamCommand('/new 现在'),null);
  assert.equal(parseTeamCommand('普通消息'),null);
  assert.equal(parseTeamCommand(''),null);
});

test('bare /compress follows the configured default level',()=>{
  same(extract().parseCompactCommand('/compress'),{kind:'native',scope:'main'});
  same(extract({defaultCompact:'bare'}).parseCompactCommand('/compress'),{kind:'native',scope:'bare'});
  same(extract({defaultCompact:'gateway-full'}).parseCompactCommand('/compress'),{kind:'gateway',scope:'full'});
  same(extract({defaultCompact:'latest'}).parseCompactCommand('/compact'),{kind:'native',scope:'latest'});
  // 显式范围始终覆盖默认档；未知默认档回退 main。
  same(extract({defaultCompact:'bare'}).parseCompactCommand('/compress full'),{kind:'native',scope:'full'});
  same(extract({defaultCompact:'nope'}).defaultCompactPreference(),'main');
});

test('native compaction appends optional instructions without changing the bare prompt',()=>{
  const {nativeCompactPrompt}=extract();
  assert.equal(nativeCompactPrompt('bare',''),'/compact');
  assert.equal(nativeCompactPrompt('main','  继续补测试  '),'/compact 保留主要历史、当前目标、关键决策和最新结果，重点压缩执行过程与工具输出。\n\n压缩完成后，请按以下用户指示继续：\n继续补测试');
});

test('plain chats parse /new as new-chat, /new stays un-interceptable via compact',()=>{
  const {parsePlainCommand,parseCompactCommand}=extract();
  same(parsePlainCommand('/new'),{kind:'newchat'});
  assert.equal(parsePlainCommand('/new 顺便'),null);
  same(parsePlainCommand('/compress'),{kind:'native',scope:'main'});
  assert.equal(parseCompactCommand('/new'),null);
});

test('plain /new creates a fresh title and inherits settings only',async()=>{
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const parseStart=source.indexOf('function parsePlainCommand'),parseEnd=source.indexOf('function compactInstructions',parseStart);
  const submitStart=source.indexOf("$('composer').onsubmit="),submitEnd=source.indexOf("$('message').onkeydown",submitStart);
  const nodes={composer:{},message:{value:'/new'}},calls=[],opened=[];
  const context={
    $:id=>nodes[id],t:value=>value,currentId:'old-chat',currentHost:'local',currentTeam:null,sending:false,
    state:{agent:'kimi',cwd:'/work',title:'旧对话标题'},closeSkillSuggestions(){},syncSendButton(){},
    attachments:{ready:()=>true,ids:()=>[]},uuid:()=> 'request-1',chatKey:(id,host)=>host+'|'+id,
    sessionStorage:{removeItem(){}},loadList:async()=>{},openChat:async(...args)=>opened.push(args),toast(){},
    api:async(path,body)=>{calls.push({path,body});return{id:'fresh-chat',host:'local'};},
  };
  vm.runInNewContext(source.slice(parseStart,parseEnd)+'\n'+source.slice(submitStart,submitEnd),context);
  await nodes.composer.onsubmit({preventDefault(){}});
  assert.equal(calls.length,1);
  assert.equal(calls[0].path,'/api/sessions');
  same(calls[0].body,{id:'request-1',agent:'kimi',cwd:'/work',title:'新聊天',inheritFrom:'old-chat'});
  assert.notEqual(calls[0].body.title,context.state.title);
  same(opened,[['fresh-chat','local']]);
});

test('goal command parses objectives and is suggested only for capable plain chats',()=>{
  const flow=extract();
  same(flow.parsePlainCommand('/goal'),{kind:'goal',objective:''});
  same(flow.parsePlainCommand(' /goal  完成迁移\n并运行测试 '),{kind:'goal',objective:'完成迁移\n并运行测试'});
  assert.equal(flow.parsePlainCommand('/goalkeeper'),null);
  assert.equal(extract({team:true}).parseTeamCommand('/goal 团队目标'),null);
  flow.state={agent:'kimi',agents:[],host:'local',capabilities:['goals'],goalRuntimeAvailable:true};
  const rows=flow.composerCommandRows('goal');
  assert.equal(rows.length,1);
  assert.equal(rows[0].token,'/goal ');
  assert.equal(rows[0].label,'设定目标');
  flow.state.capabilities=[];
  assert.equal(flow.composerCommandRows('goal').length,0);
});

test('migrate command parses long and shorthand targets with multiline instructions',()=>{
  const flow=extract({team:true});
  flow.state={agent:'kimi',agents:[{kind:'kimi'},{kind:'zcode'},{kind:'codex'}]};
  same(flow.parsePlainCommand('/migrate zcode'),{kind:'migrate',agent:'zcode',scope:'main'});
  same(flow.parsePlainCommand('/migrate zcode full 继续补测试\n并修复失败项'),{kind:'migrate',agent:'zcode',scope:'full',instructions:'继续补测试\n并修复失败项'});
  same(flow.parsePlainCommand('/migrate'),{kind:'migrate',agent:'',scope:'main'});
  same(flow.parsePlainCommand('/zcode'),{kind:'migrate',agent:'zcode',scope:'main'});
  same(flow.parsePlainCommand('/zcode latest 继续补测试'),{kind:'migrate',agent:'zcode',scope:'latest',instructions:'继续补测试'});
  same(flow.parsePlainCommand('/codex main 接着实现'),{kind:'migrate',agent:'codex',scope:'main',instructions:'接着实现'});
  same(flow.parsePlainCommand('/kimi full 压缩后继续'),{kind:'migrate',agent:'kimi',scope:'full',instructions:'压缩后继续'});
  assert.equal(flow.parsePlainCommand('/unknown 普通消息'),null);
});

test('migrate command suggestions include the current agent as gateway compaction',()=>{
  const flow=extract();
  flow.state={agent:'kimi',agents:[{kind:'kimi',label:'Kimi'},{kind:'zcode',label:'ZCode'},{kind:'codex',label:'Codex'}]};
  const rows=flow.composerCommandRows('migrate');
  same(rows.map(row=>row.token),['/kimi','/zcode','/codex']);
  assert.match(rows[0].label,/当前 Agent 压缩并新建会话/);
  assert.match(rows[0].description,/网关创建压缩新会话并打开/);
  assert.match(rows[1].label,/ZCode/);
  assert.match(rows[1].description,/后续指示/);
});

test('transcript migration and gateway compaction allow an active source with pending requests',async()=>{
  const flow=extract();
  flow.state={id:'chat',agent:'kimi',status:'active',connected:true,readOnly:false,requests:[{id:'approval'}]};
  assert.equal(flow.transcriptMaintenanceAvailable(),true);
  assert.equal(flow.compactAvailable(),false);
  flow.state.status='idle';
  assert.equal(flow.transcriptMaintenanceAvailable(),true);
  assert.equal(flow.compactAvailable(),false);
  flow.state.requests=[];
  assert.equal(flow.compactAvailable(),true);
  flow.state.connected=false;
  assert.equal(flow.transcriptMaintenanceAvailable(),true);
  assert.equal(flow.compactAvailable(),false);
  flow.state.connected=true;flow.state.readOnly=true;
  assert.equal(flow.transcriptMaintenanceAvailable(),true);
  assert.equal(flow.compactAvailable(),false);

  const calls=[];
  flow.currentId='chat';flow.currentTeam=null;flow.$=()=>({textContent:''});
  flow.migrateControl={migrate:async(...args)=>{calls.push(args);return true;}};
  flow.state.readOnly=false;flow.state.status='active';flow.state.requests=[{id:'approval'}];
  assert.equal(await flow.runGatewayCompact('main','继续实现'),true);
  same(calls,[['kimi','main','compact',{instructions:'继续实现'}]]);
});

test('migrate command validates the target and delegates with instructions',async()=>{
  const flow=extract(),calls=[],messages=[];
  flow.toast=message=>messages.push(message);
  flow.state={id:'chat',agent:'kimi',status:'active',connected:true,readOnly:false,requests:[{id:'approval'}],agents:[{kind:'kimi'},{kind:'zcode'}]};
  flow.migrateControl={migrate:async(...args)=>{calls.push(args);return true;}};
  assert.equal(await flow.runMigrateCommand({agent:'',scope:'main'}),false);
  assert.equal(await flow.runMigrateCommand({agent:'codex',scope:'main'}),false);
  assert.equal(calls.length,0);
  assert.match(messages[0],/^用法：\/migrate/);
  assert.match(messages[1],/未启用/);
  assert.equal(await flow.runMigrateCommand({agent:'kimi',scope:'full',instructions:'压缩后继续'}),true);
  assert.equal(await flow.runMigrateCommand({agent:'zcode',scope:'latest',instructions:'继续完成剩余测试'}),true);
  same(calls,[
    ['kimi','full','compact',{instructions:'压缩后继续'}],
    ['zcode','latest','migrate',{instructions:'继续完成剩余测试'}],
  ]);
});

test('command suggestions follow the chat mode',()=>{
  const team=extract({team:true}),plain=extract({team:false});
  const teamRows=team.composerCommandRows(''),plainRows=plain.composerCommandRows('');
  assert.equal(teamRows.length,0);
  assert.equal(plainRows.length,10);
  assert.equal(plainRows[0].token,'/new');
  assert.match(plainRows[0].label,/新建会话/);
  // 默认档行展示当前生效档位。
  const customized=extract({defaultCompact:'gateway-full'}).composerCommandRows('')[2];
  assert.equal(customized.token,'/compress');
  assert.match(customized.description,/网关 · 完整/);
  // 网关三档齐全，描述按模式区分「换绑主控/并打开」。
  const gateways=plainRows.filter(row=>row.token.startsWith('/compress gateway'));
  assert.equal(gateways.length,3);
  assert.match(gateways.find(row=>row.token==='/compress gateway full').label,/完整/);
  assert.match(gateways[0].description,/并打开/);
  assert.equal(team.composerCommandRows('gateway').length,0);
  // 前缀与别名过滤。
  assert.equal(plain.composerCommandRows('compress').length,8);
  assert.equal(plain.composerCommandRows('compact').length,8);
  assert.equal(plain.composerCommandRows('gateway').length,3);
  assert.equal(plain.composerCommandRows('bare').length,1);
  assert.equal(plain.composerCommandRows('nothing').length,0);
  assert.ok(plainRows.every(row=>row.label&&row.description),'rows need visible label and description');
});

function extractNewContext({runs=[],confirm=true,cancelError}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('async function teamContextState(');
  const end=source.indexOf('// 命令面板只服务普通会话',start);
  assert.ok(start>=0&&end>start,'team /new flow not found in app.js');
  const calls=[];
  const context={
    t:value=>value,encodeURIComponent,TEAM_TERMINAL_RUN_STATUSES:new Set(['completed','failed','cancelled','stopped','outcome_unknown']),
    window:{confirm:message=>{calls.push(['confirm',message]);return confirm;}},
    teamChat:{
      refreshRun:()=>calls.push(['refresh']),
      resetWorkflow:(teamId,rows)=>calls.push(['reset',teamId,rows.map(run=>run.id)]),
    },
    teamRequest:async(path,options)=>{
      calls.push(['request',path,options]);
      if(path.endsWith('/runs'))return{runs};
      if(path.endsWith('/inputs?limit=1'))return{pendingCount:0};
      if(cancelError)throw cancelError;
      return{status:'cancelled'};
    },
    leadContextAction:async()=>{calls.push(['new']);return{sessionId:'fresh'};},
  };
  vm.runInNewContext(source.slice(start,end)+'\nthis.newTeamLeadContext=newTeamLeadContext;',context);
  return{run:()=>context.newTeamLeadContext({id:'team'},'old'),calls};
}

test('team /new cancels a confirmed parked run before switching lead',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'waiting_input',version:7}]});
  assert.equal((await flow.run()).sessionId,'fresh');
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','request','confirm','request','refresh','new','reset']);
  assert.deepEqual(JSON.parse(JSON.stringify(flow.calls[3])),['request','/api/teams/team/runs/run-1/cancel-parked',{method:'POST',body:{version:7}}]);
  assert.deepEqual(flow.calls[6],['reset','team',['run-1']]);
  assert.match(flow.calls[2][1],/取消该 Run/);
});

test('team /new keeps a parked run when confirmation is declined',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'paused',version:4}],confirm:false});
  assert.equal(await flow.run(),null);
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','request','confirm']);
});

test('team /new refuses an active run without cancelling it',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'dispatching',version:3}]});
  await assert.rejects(flow.run(),/仍在执行/);
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','request']);
});

test('team /new cannot cancel a parked run while TeamInputs are pending',async()=>{
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('async function teamContextState('),end=source.indexOf('// 命令面板只服务普通会话',start),calls=[];
  const context={
    t:value=>value,encodeURIComponent,TEAM_TERMINAL_RUN_STATUSES:new Set(['completed','failed','cancelled','stopped','outcome_unknown']),
    window:{confirm:()=>{calls.push(['confirm']);return true;}},
    teamRequest:async path=>{calls.push(['request',path]);return path.endsWith('/runs')?{runs:[{id:'run-1',status:'paused',version:2}]}:{pendingCount:1};},
    teamChat:{refreshRun(){calls.push(['refresh']);}},leadContextAction:async()=>{calls.push(['new']);},
  };
  vm.runInNewContext(source.slice(start,end)+'\nthis.newTeamLeadContext=newTeamLeadContext;',context);
  await assert.rejects(context.newTeamLeadContext({id:'team'},'lead'),/待路由或等待处理/);
  assert.deepEqual(calls.map(call=>call[0]),['request','request']);
});

test('team /new does not switch lead when parked cancellation fails',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'waiting_input',version:7}],cancelError:Error('版本冲突')});
  await assert.rejects(flow.run(),/版本冲突/);
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','request','confirm','request']);
});
