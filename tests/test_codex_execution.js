'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function fixture(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('let switchingExecution=false;'),end=source.indexOf("$('reconnect').onclick=",start);
  const nodes=new Map(),calls=[];
  const context={currentId:'original',currentHost:'local',state:{agent:'codex',host:'local',executionTarget:'desktop',executionOwner:'',executionSwitchSupported:true},sending:false,
    $:id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',hidden:false,disabled:false});return nodes.get(id);},
    t:value=>value,chatProjectLabel:()=> 'Project',usesDesktopRuntime:view=>view.executionTarget==='desktop',
    sessionUrl:(id,action)=>id+'/'+action,api:async(url,payload)=>{calls.push({url,payload});return context.state;},
    renderState:view=>{context.state=view;},toast:()=>{},
  };
  vm.createContext(context);vm.runInContext(source.slice(start,end),context);
  return {context,nodes,calls};
}

test('execution target stays visible without implying a connected writer',()=>{
  const {context,nodes}=fixture();
  context.state.executionTarget='cli';context.renderExecutionState(context.state);
  assert.match(nodes.get('execution-status').textContent,/网关 Codex CLI.*未连接/);
  assert.equal(nodes.get('execution-switch').textContent,'交给桌面执行');
});

test('handoff sends only the execution operation and preserves the message draft',async()=>{
  const {context,nodes,calls}=fixture();
  context.$('message').value='unsent draft';
  await nodes.get('execution-switch').onclick();
  assert.equal(calls[0].url,'original/execution');
  assert.equal(calls[0].payload.target,'cli');
  assert.equal(calls.some(call=>call.url.endsWith('/send')||call.url.endsWith('/reconnect')),false);
  assert.equal(nodes.get('message').value,'unsent draft');
});

test('handoff completion cannot overwrite a newer chat selection',async()=>{
  const {context,nodes}=fixture();let complete;
  context.api=()=>new Promise(resolve=>{complete=resolve;});
  const operation=nodes.get('execution-switch').onclick();
  context.currentId='newer';context.state={...context.state,title:'newer'};
  complete({executionTarget:'cli'});await operation;
  assert.equal(context.state.title,'newer');
});

test('running, approval and queued or unknown messages disable handoff',()=>{
  const {context,nodes}=fixture();
  for(const patch of [{status:'active'},{requests:[{}]},{submissions:[{status:'queued'}]},{submissions:[{status:'unknown'}]}]){
    context.renderExecutionState({...context.state,...patch});
    assert.equal(nodes.get('execution-switch').disabled,true);
  }
  context.renderExecutionState({...context.state,submissions:[{status:'failed'}]});
  assert.equal(nodes.get('execution-switch').disabled,false);
});

test('ACP and remote chats do not expose Codex handoff',()=>{
  const {context,nodes}=fixture();
  for(const patch of [{agent:'kimi'},{host:'ssh'},{executionSwitchSupported:false}]){
    context.renderExecutionState({...context.state,...patch});
    assert.equal(nodes.get('execution-control').hidden,true);
  }
});
