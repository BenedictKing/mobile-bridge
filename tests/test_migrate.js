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

const CATALOG={models:[
  {id:'m1',name:'Model One',efforts:['low','high'],defaultEffort:'low'},
  {id:'m2',name:'Model Two',efforts:['medium','xhigh'],defaultEffort:'xhigh'}],currentEffort:'high'};
const find=(root,selector)=>root.querySelectorAll('button').find(selector);
function byClass(node,name,found=[]){if(node.className&&node.className.split(' ').includes(name))found.push(node);for(const child of node.children)byClass(child,name,found);return found;}

test('scope click prepares the target, then confirm migrates with the chosen settings',async()=>{
  const requests=[];
  const {control,root,completed}=loadControl({request:async body=>{requests.push(body);return body.phase==='prepare'?{catalog:CATALOG}:{sent:true};}});
  find(root,button=>button.dataset.agent==='codex').onclick();
  await find(root,button=>button.dataset.scope==='main').onclick();
  assert.equal(requests.length,1);
  assert.equal(requests[0].phase,'prepare');
  assert.equal(requests[0].agent,'codex');
  assert.equal(requests[0].scope,'main');
  assert.equal(completed.length,0);  // Nothing is migrated before the settings step.

  const [model]=byClass(root,'migrate-model'),[effort]=byClass(root,'migrate-effort');
  assert.deepEqual(Array.from(model.children,option=>option.value),['','m1','m2']);
  assert.equal(effort.disabled,true);  // No model chosen: the target keeps its defaults.
  model.value='m2';model.onchange();
  assert.deepEqual(Array.from(effort.children,option=>option.value),['medium','xhigh']);
  assert.equal(effort.value,'xhigh');  // The picked model's default effort.

  await byClass(root,'migrate-start')[0].onclick();
  assert.equal(requests.length,2);
  assert.equal(requests[1].phase,undefined);
  assert.equal(requests[1].id,requests[0].id);  // Confirm reuses the prepared session.
  assert.equal(requests[1].model,'m2');
  assert.equal(requests[1].effort,'xhigh');
  assert.equal(completed[0][3],'migrate');
});

test('migrating on target defaults sends no model or effort',async()=>{
  const requests=[];
  const {root}=loadControl({request:async body=>{requests.push(body);return body.phase==='prepare'?{catalog:CATALOG}:{sent:true};}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  await find(root,button=>button.dataset.scope==='full').onclick();
  await byClass(root,'migrate-start')[0].onclick();
  assert.equal('model' in requests[1],false);
  assert.equal('effort' in requests[1],false);
});

test('an empty or failed catalog still migrates, on the target defaults',async()=>{
  const requests=[];
  const {root}=loadControl({request:async body=>{requests.push(body);return body.phase==='prepare'?{catalog:{models:[],catalogError:'boom'}}:{sent:true};}});
  find(root,button=>button.dataset.agent==='codex').onclick();
  await find(root,button=>button.dataset.scope==='main').onclick();
  assert.equal(byClass(root,'migrate-model').length,0);
  await byClass(root,'migrate-start')[0].onclick();
  assert.equal(requests.length,2);
  assert.equal('model' in requests[1],false);
});

test('going back from the settings step abandons the prepared session',async()=>{
  const requests=[];
  const {control,root}=loadControl({request:async body=>{requests.push(body);return {catalog:CATALOG};}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  await find(root,button=>button.dataset.scope==='latest').onclick();
  byClass(root,'migrate-back')[0].onclick();
  assert.equal(requests.length,2);
  assert.equal(requests[1].phase,'abandon');
  assert.equal(requests[1].id,requests[0].id);
  assert.equal(requests[1].agent,'kimi');
  assert.equal(control.prepared,null);
  assert.equal(byClass(root,'migrate-scope').length,3);  // Back on the scope step.
});

test('a rejected confirm keeps the settings step so another model can be picked',async()=>{
  const errors=[],requests=[];
  const {control,root}=loadControl({
    onError:error=>errors.push(error.message),
    request:async body=>{requests.push(body);if(body.phase==='prepare')return {catalog:CATALOG};throw Error('此 Agent 不支持所选模型');}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  await find(root,button=>button.dataset.scope==='main').onclick();
  const [model]=byClass(root,'migrate-model');model.value='m1';model.onchange();
  await byClass(root,'migrate-start')[0].onclick();
  assert.deepEqual(errors,['此 Agent 不支持所选模型']);
  assert.notEqual(control.prepared,null);
  assert.equal(byClass(root,'migrate-start').length,1);
  assert.equal(byClass(root,'migrate-model')[0].value,'m1');  // The choice survives the error.
});

test('switching to another chat abandons a prepared session',async()=>{
  const requests=[];
  const {control,root}=loadControl({request:async body=>{requests.push(body);return {catalog:CATALOG};}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  await find(root,button=>button.dataset.scope==='main').onclick();
  control.render({id:'other',agent:'claude',agents:[{kind:'claude'},{kind:'codex'},{kind:'kimi'}]});
  assert.equal(requests.at(-1).phase,'abandon');
  assert.equal(control.prepared,null);
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

test('dismissing the dialog abandons a prepared session and resets the picker',async()=>{
  const requests=[];
  const {control,root}=loadControl({request:async body=>{requests.push(body);return {catalog:CATALOG};}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  await find(root,button=>button.dataset.scope==='main').onclick();
  control.dismiss();
  assert.equal(requests.at(-1).phase,'abandon');
  assert.equal(control.picked,null);
  assert.deepEqual(root.querySelectorAll('button').map(button=>button.dataset.agent),['codex','kimi']);
});

test('dismissing while the target is still being prepared abandons it once ready',async()=>{
  let release;
  const requests=[];
  const {control,root}=loadControl({request:body=>{requests.push(body);return body.phase==='prepare'?new Promise(resolve=>{release=()=>resolve({catalog:CATALOG});}):Promise.resolve({});}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  const pending=find(root,button=>button.dataset.scope==='main').onclick();
  control.dismiss();
  release();
  await pending;
  assert.equal(control.prepared,null);
  assert.equal(requests.length,2);
  assert.equal(requests[1].phase,'abandon');
  assert.equal(requests[1].id,requests[0].id);
});

test('dismissing during a running confirm never abandons the session being sent',async()=>{
  let release;
  const requests=[];
  const {control,root}=loadControl({request:body=>{requests.push(body);return body.phase==='prepare'?Promise.resolve({catalog:CATALOG}):new Promise(resolve=>{release=()=>resolve({sent:true});});}});
  find(root,button=>button.dataset.agent==='kimi').onclick();
  await find(root,button=>button.dataset.scope==='main').onclick();
  const running=byClass(root,'migrate-start')[0].onclick();
  control.dismiss();
  release();
  await running;
  assert.deepEqual(requests.map(body=>body.phase),['prepare',undefined]);
});
