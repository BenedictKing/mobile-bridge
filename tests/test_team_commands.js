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
  const end=source.indexOf('async function runGatewayCompact',start);
  assert.ok(start>=0&&end>start,'composer command block not found in app.js');
  const context={t:value=>value,currentTeam:team,state:{agent:'kimi',agents:[{kind:'kimi'}]},window:{BridgePresentation:{defaultCompact:()=>defaultCompact}}};
  vm.runInNewContext(source.slice(start,end)+'\nthis.composerCommandRows=composerCommandRows;this.parseTeamCommand=parseTeamCommand;this.parseCompactCommand=parseCompactCommand;this.parsePlainCommand=parsePlainCommand;this.parseMigrateCommand=parseMigrateCommand;this.runMigrateCommand=runMigrateCommand;this.resolveDefaultCompact=resolveDefaultCompact;this.defaultCompactPreference=defaultCompactPreference;this.nativeCompactPrompt=nativeCompactPrompt;',context);
  return context;
}
const same=(actual,expected)=>assert.deepEqual(JSON.parse(JSON.stringify(actual)),expected);

test('parseTeamCommand handles /new exactly and passes compaction instructions',()=>{
  const {parseTeamCommand}=extract({team:true});
  same(parseTeamCommand('/new'),{kind:'new'});
  same(parseTeamCommand('  /new \n'),{kind:'new'});
  // /new 带附加文字仍是普通团队消息；压缩命令的附加文字是后续指示。
  assert.equal(parseTeamCommand('/new 现在'),null);
  same(parseTeamCommand('/compress main 立刻'),{kind:'native',scope:'main',instructions:'立刻'});
  same(parseTeamCommand('/compress gateway full 继续补测试\n并修复失败项'),{kind:'gateway',scope:'full',instructions:'继续补测试\n并修复失败项'});
  same(parseTeamCommand('/compress 接着实现'),{kind:'native',scope:'main',instructions:'接着实现'});
  assert.equal(parseTeamCommand('普通消息'),null);
  assert.equal(parseTeamCommand(''),null);
  // /compact 是 /compress 的等价写法；裸命令执行默认档。
  same(parseTeamCommand('/compress'),{kind:'native',scope:'main'});
  same(parseTeamCommand('/compress bare'),{kind:'native',scope:'bare'});
  same(parseTeamCommand('/compress gateway'),{kind:'gateway',scope:'main'});
  same(parseTeamCommand('/compact gateway full'),{kind:'gateway',scope:'full'});
  same(parseTeamCommand('/compress gateway latest'),{kind:'gateway',scope:'latest'});
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

test('migrate command parses long and shorthand targets with multiline instructions',()=>{
  const flow=extract({team:true});
  flow.state={agent:'kimi',agents:[{kind:'kimi'},{kind:'zcode'},{kind:'codex'}]};
  same(flow.parsePlainCommand('/migrate zcode'),{kind:'migrate',agent:'zcode',scope:'main'});
  same(flow.parsePlainCommand('/migrate zcode full 继续补测试\n并修复失败项'),{kind:'migrate',agent:'zcode',scope:'full',instructions:'继续补测试\n并修复失败项'});
  same(flow.parseTeamCommand('/migrate codex latest'),{kind:'migrate',agent:'codex',scope:'latest'});
  same(flow.parsePlainCommand('/migrate'),{kind:'migrate',agent:'',scope:'main'});
  same(flow.parsePlainCommand('/zcode'),{kind:'migrate',agent:'zcode',scope:'main'});
  same(flow.parsePlainCommand('/zcode latest 继续补测试'),{kind:'migrate',agent:'zcode',scope:'latest',instructions:'继续补测试'});
  same(flow.parseTeamCommand('/codex main 接着实现'),{kind:'migrate',agent:'codex',scope:'main',instructions:'接着实现'});
  assert.equal(flow.parsePlainCommand('/unknown 普通消息'),null);
});

test('migrate command suggestions list enabled targets except the current agent',()=>{
  const flow=extract();
  flow.state={agent:'kimi',agents:[{kind:'kimi',label:'Kimi'},{kind:'zcode',label:'ZCode'},{kind:'codex',label:'Codex'}]};
  const rows=flow.composerCommandRows('migrate');
  same(rows.map(row=>row.token),['/zcode','/codex']);
  assert.match(rows[0].label,/ZCode/);
  assert.match(rows[0].description,/后续指示/);
});

test('migrate command validates the target and delegates with instructions',async()=>{
  const flow=extract(),calls=[],messages=[];
  flow.toast=message=>messages.push(message);
  flow.compactAvailable=()=>true;
  flow.state={agent:'kimi',agents:[{kind:'kimi'},{kind:'zcode'}]};
  flow.migrateControl={migrate:async(...args)=>calls.push(args)};
  assert.equal(await flow.runMigrateCommand({agent:'',scope:'main'}),false);
  assert.equal(await flow.runMigrateCommand({agent:'kimi',scope:'main'}),false);
  assert.equal(await flow.runMigrateCommand({agent:'codex',scope:'main'}),false);
  assert.equal(calls.length,0);
  assert.match(messages[0],/^用法：\/migrate/);
  assert.match(messages[1],/不能是当前 Agent/);
  assert.match(messages[2],/未启用/);
  assert.equal(await flow.runMigrateCommand({agent:'zcode',scope:'latest',instructions:'继续完成剩余测试'}),true);
  same(calls,[['zcode','latest','migrate',{instructions:'继续完成剩余测试'}]]);
});

test('command suggestions follow the chat mode',()=>{
  const team=extract({team:true}),plain=extract({team:false});
  // 九行：/new、默认档、原生 bare/full/main/latest、网关 main/full/latest。
  const teamRows=team.composerCommandRows(''),plainRows=plain.composerCommandRows('');
  assert.equal(teamRows.length,9);
  assert.equal(plainRows.length,9);
  assert.equal(teamRows[0].token,'/new');
  assert.match(teamRows[0].label,/更换空白主控/);
  assert.equal(plainRows[0].token,'/new');
  assert.match(plainRows[0].label,/新建会话/);
  // 默认档行展示当前生效档位。
  const customized=extract({team:true,defaultCompact:'gateway-full'}).composerCommandRows('')[1];
  assert.equal(customized.token,'/compress');
  assert.match(customized.description,/网关 · 完整/);
  // 网关三档齐全，描述按模式区分「换绑主控/并打开」。
  const gateways=plainRows.filter(row=>row.token.startsWith('/compress gateway'));
  assert.equal(gateways.length,3);
  assert.match(gateways.find(row=>row.token==='/compress gateway full').label,/完整/);
  assert.match(gateways[0].description,/并打开/);
  assert.match(team.composerCommandRows('gateway')[0].description,/换绑主控/);
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
  const start=source.indexOf('async function newTeamLeadContext(');
  const end=source.indexOf('// 命令面板：',start);
  assert.ok(start>=0&&end>start,'team /new flow not found in app.js');
  const calls=[];
  const context={
    t:value=>value,encodeURIComponent,TEAM_TERMINAL_RUN_STATUSES:new Set(['completed','failed','cancelled','stopped','outcome_unknown']),
    window:{confirm:message=>{calls.push(['confirm',message]);return confirm;}},
    teamChat:{refreshRun:()=>calls.push(['refresh'])},
    teamRequest:async(path,options)=>{
      calls.push(['request',path,options]);
      if(path.endsWith('/runs'))return{runs};
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
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','confirm','request','refresh','new']);
  assert.deepEqual(JSON.parse(JSON.stringify(flow.calls[2])),['request','/api/teams/team/runs/run-1/cancel-parked',{method:'POST',body:{version:7}}]);
  assert.match(flow.calls[1][1],/取消该 Run/);
});

test('team /new keeps a parked run when confirmation is declined',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'paused',version:4}],confirm:false});
  assert.equal(await flow.run(),null);
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','confirm']);
});

test('team /new refuses an active run without cancelling it',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'dispatching',version:3}]});
  await assert.rejects(flow.run(),/仍在执行/);
  assert.deepEqual(flow.calls.map(call=>call[0]),['request']);
});

test('team /new does not switch lead when parked cancellation fails',async()=>{
  const flow=extractNewContext({runs:[{id:'run-1',status:'waiting_input',version:7}],cancelError:Error('版本冲突')});
  await assert.rejects(flow.run(),/版本冲突/);
  assert.deepEqual(flow.calls.map(call=>call[0]),['request','confirm','request']);
});
