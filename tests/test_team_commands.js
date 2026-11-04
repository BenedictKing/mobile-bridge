'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

// 从 app.js 抽出团队命令面板的纯函数段（与 test_queue.js 的抽取惯例一致）。
function extract(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function teamCommandRows(');
  const end=source.indexOf('// runNativeCompact',start);
  assert.ok(start>=0&&end>start,'team command block not found in app.js');
  const context={t:value=>value};
  vm.runInNewContext(source.slice(start,end)+'\nthis.teamCommandRows=teamCommandRows;this.parseTeamCommand=parseTeamCommand;',context);
  return context;
}

test('parseTeamCommand only intercepts exact command lines',()=>{
  const {parseTeamCommand}=extract();
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

test('team command suggestions filter by prefix and alias',()=>{
  const {teamCommandRows}=extract();
  const all=teamCommandRows('');
  assert.equal(all.length,5);
  assert.equal(all[0].token,'/new');
  assert.ok(all[0].label&&all[0].description,'rows need visible label and description');
  assert.equal(teamCommandRows('new').length,1);
  assert.equal(teamCommandRows('com').length,4);
  // 输入 /compact 前缀也要能匹配 /compress 系命令（别名检索）。
  assert.equal(teamCommandRows('compact').length,4);
  const gateway=teamCommandRows('gateway');
  assert.equal(gateway.length,1);
  assert.equal(gateway[0].token,'/compress gateway');
  assert.equal(teamCommandRows('nothing').length,0);
});
