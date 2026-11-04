'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

// 从 app.js 抽出命令面板的纯函数段（与 test_queue.js 的抽取惯例一致）。
// defaultCompact 模拟设置里的默认压缩档（BridgePresentation.defaultCompact）。
function extract({team=false,defaultCompact}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('const COMPACT_PREF_LABELS=');
  const end=source.indexOf('// runNativeCompact',start);
  assert.ok(start>=0&&end>start,'composer command block not found in app.js');
  const context={t:value=>value,currentTeam:team,window:{BridgePresentation:{defaultCompact:()=>defaultCompact}}};
  vm.runInNewContext(source.slice(start,end)+'\nthis.composerCommandRows=composerCommandRows;this.parseTeamCommand=parseTeamCommand;this.parseCompactCommand=parseCompactCommand;this.parsePlainCommand=parsePlainCommand;this.resolveDefaultCompact=resolveDefaultCompact;this.defaultCompactPreference=defaultCompactPreference;',context);
  return context;
}
const same=(actual,expected)=>assert.deepEqual(JSON.parse(JSON.stringify(actual)),expected);

test('parseTeamCommand only intercepts exact command lines',()=>{
  const {parseTeamCommand}=extract({team:true});
  same(parseTeamCommand('/new'),{kind:'new'});
  same(parseTeamCommand('  /new \n'),{kind:'new'});
  // 带附加文字的按普通团队消息处理，绝不截断成命令。
  assert.equal(parseTeamCommand('/new 现在'),null);
  assert.equal(parseTeamCommand('/compress main 立刻'),null);
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

test('plain chats parse /new as new-chat, /new stays un-interceptable via compact',()=>{
  const {parsePlainCommand,parseCompactCommand}=extract();
  same(parsePlainCommand('/new'),{kind:'newchat'});
  assert.equal(parsePlainCommand('/new 顺便'),null);
  same(parsePlainCommand('/compress'),{kind:'native',scope:'main'});
  assert.equal(parseCompactCommand('/new'),null);
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
