'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode {
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.hidden=false;this.disabled=false;}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  querySelectorAll(selector){
    const matches=[];
    const visit=node=>{if(selector==='button'&&node.tag==='button')matches.push(node);for(const child of node.children)visit(child);};
    visit(this);
    return matches;
  }
}

function loadControl(overrides={}){
  const context={
    BridgeI18n:{t:value=>value,locale:()=>'zh-CN',agentLabel:value=>value},
    crypto:{randomUUID:()=>'request-id'},
    document:{createElement:tag=>new FakeNode(tag)}
  };
  vm.runInNewContext(readFileSync(require.resolve('../web/migrate.js'),'utf8')+'\nthis.MigrateControl=MigrateControl;',context);
  const root=new FakeNode('div'),requests=[],busy=[],completed=[];
  const control=new context.MigrateControl({
    root,
    request:async body=>{requests.push(body);return {sent:true};},
    confirm:()=>true,
    onMigrated:(...args)=>completed.push(args),
    onBusy:value=>busy.push(value),
    ...overrides
  });
  const view={id:'source',agent:'claude',agents:[{kind:'claude'},{kind:'codex'},{kind:'kimi'}]};
  control.render(view);
  return {control,root,requests,busy,completed,view};
}

test('migration targets exclude the current agent',()=>{
  const {control,root}=loadControl();
  assert.deepEqual(Array.from(control.targets(),agent=>agent.kind),['codex','kimi']);
  assert.deepEqual(root.querySelectorAll('button').map(button=>button.dataset.agent),['codex','kimi']);
  assert.equal(root.children[0].textContent,'迁移到其他 Agent');
});

test('migration sends the picked target and scope',async()=>{
  const {control,root,requests,completed}=loadControl();
  root.querySelectorAll('button').find(button=>button.dataset.agent==='codex').onclick();
  await root.querySelectorAll('button').find(button=>button.dataset.scope==='main').onclick();
  assert.equal(requests.length,1);
  assert.equal(requests[0].agent,'codex');
  assert.equal(requests[0].scope,'main');
  assert.equal(completed[0][3],'migrate');
});

test('gateway compaction keeps its operation type and rejects duplicate submission',async()=>{
  let release;
  const {control,requests,busy,completed}=loadControl({request:body=>{requests.push(body);return new Promise(resolve=>{release=resolve;});}});
  const pending=control.migrate('claude','latest','compact');
  await control.migrate('claude','full','compact');
  assert.equal(control.busy,true);
  assert.equal(requests.length,1);
  assert.equal(requests[0].agent,'claude');
  assert.equal(requests[0].scope,'latest');
  release({sent:true});
  await pending;
  assert.equal(completed[0][3],'compact');
  assert.deepEqual(busy,[true,false]);
  assert.equal(control.busy,false);
});

test('chat details keep migration before and outside the compaction section',()=>{
  const html=readFileSync(require.resolve('../web/index.html'),'utf8');
  const compact=html.match(/<section id="compact-control"[\s\S]*?<\/section>/)[0];
  assert(!compact.includes('id="migrate-control"'));
  assert.equal((compact.match(/data-gateway-compact=/g)||[]).length,3);
  assert(compact.includes('在当前 Agent 创建转录压缩的新会话'));
  assert(html.indexOf('id="migrate-control"')<html.indexOf('id="compact-control"'));
});
