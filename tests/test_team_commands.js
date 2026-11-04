'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

// 从 app.js 抽出命令面板的纯函数段（与 test_queue.js 的抽取惯例一致）。
// composerCommandRows 读取全局 currentTeam：团队模式含 /new，普通会话只有压缩。
function extract({team=false}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function composerCommandRows(');
  const end=source.indexOf('// runNativeCompact',start);
  assert.ok(start>=0&&end>start,'composer command block not found in app.js');
  const context={t:value=>value,currentTeam:team};
  vm.runInNewContext(source.slice(start,end)+'\nthis.composerCommandRows=composerCommandRows;this.parseTeamCommand=parseTeamCommand;this.parseCompactCommand=parseCompactCommand;this.parsePlainCommand=parsePlainCommand;',context);
  return context;
}

test('parseTeamCommand only intercepts exact command lines',()=>{
  const {parseTeamCommand}=extract({team:true});
  // vm 内构造的对象与主 realm 原型不同，deepStrictEqual 会误报：先 JSON 规整。
  const same=(actual,expected)=>assert.deepEqual(JSON.parse(JSON.stringify(actual)),expected);
  same(parseTeamCommand('/new'),{kind:'new'});
  same(parseTeamCommand('  /new \n'),{kind:'new'});
  // 带附加文字的按普通团队消息处理，绝不截断成命令。
  assert.equal(parseTeamCommand('/new 现在'),null);
  assert.equal(parseTeamCommand('/compress main 立刻'),null);
  assert.equal(parseTeamCommand('普通消息'),null);
  assert.equal(parseTeamCommand(''),null);
  // /compact 是 /compress 的等价写法；裸命令默认 main。
  same(parseTeamCommand('/compress'),{kind:'native',scope:'main'});
  same(parseTeamCommand('/compact'),{kind:'native',scope:'main'});
  same(parseTeamCommand('/compress full'),{kind:'native',scope:'full'});
  same(parseTeamCommand('/compress latest'),{kind:'native',scope:'latest'});
  same(parseTeamCommand('/compress gateway'),{kind:'gateway'});
  same(parseTeamCommand('/compact gateway'),{kind:'gateway'});
});

test('compact commands parse in plain chats too, /new starts a new chat',()=>{
  const {parseCompactCommand,parsePlainCommand}=extract();
  const same=(actual,expected)=>assert.deepEqual(JSON.parse(JSON.stringify(actual)),expected);
  same(parseCompactCommand('/compress'),{kind:'native',scope:'main'});
  same(parseCompactCommand('/compact gateway'),{kind:'gateway'});
  // 普通会话的 /new 拦截为「按当前配置新建对话」，附加文字不截断。
  same(parsePlainCommand('/new'),{kind:'newchat'});
  assert.equal(parsePlainCommand('/new 顺便'),null);
  same(parsePlainCommand('/compress'),{kind:'native',scope:'main'});
});

test('command suggestions follow the chat mode',()=>{
  const team=extract({team:true}),plain=extract({team:false});
  // 两种模式都提供 /new，但语义与文案不同。
  assert.equal(team.composerCommandRows('').length,5);
  const teamNew=team.composerCommandRows('')[0],plainNew=plain.composerCommandRows('')[0];
  assert.equal(teamNew.token,'/new');
  assert.match(teamNew.label,/更换空白主控/);
  assert.equal(plain.composerCommandRows('').length,5);
  assert.equal(plainNew.token,'/new');
  assert.match(plainNew.label,/新建会话/);
  assert.match(plainNew.description,/当前配置/);
  // 网关压缩描述按模式区分「换绑主控/并打开」。
  assert.match(plain.composerCommandRows('').find(row=>row.token==='/compress gateway').description,/并打开/);
  assert.match(team.composerCommandRows('').find(row=>row.token==='/compress gateway').description,/换绑主控/);
  // 前缀与别名过滤。
  assert.equal(plain.composerCommandRows('com').length,4);
  assert.equal(plain.composerCommandRows('compact').length,4);
  assert.equal(plain.composerCommandRows('gateway').length,1);
  assert.equal(plain.composerCommandRows('nothing').length,0);
  assert.ok(plain.composerCommandRows('').every(row=>row.label&&row.description),'rows need visible label and description');
});
