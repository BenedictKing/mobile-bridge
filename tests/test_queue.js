'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const {PendingCommands,MemoryStorage}=require('./helpers/pending_commands.js');

function queueHelpers(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('let queueOrderSync=null;');
  const end=source.indexOf('// waiting_input 任务卡',start);
  assert.ok(start>=0&&end>start,'queue helpers not found');
  const context={window:{BridgePendingCommands:new PendingCommands(new MemoryStorage())},chatKey:(id,host)=>host+'|'+id};
  vm.runInNewContext(source.slice(start,end)+'\nthis.unique=uniqueQueueRows;this.move=moveQueueRows;this.render=renderQueue;this.applyPending=applyPendingQueueOrder;this.pendingOrder=()=>queueOrderSync;',context);
  return{uniqueQueueRows:context.unique,moveQueueRows:context.move,renderQueue:context.render,context};
}
const {uniqueQueueRows,moveQueueRows}=queueHelpers();

test('queue keeps only the first row for each submission id',()=>{
  const first={id:'same',text:'来自实时快照',status:'queued'};
  const rows=uniqueQueueRows([first,{id:'same',text:'本地追加',status:'queued'},{id:'other',text:'另一条',status:'queued'}]);
  assert.deepEqual(rows,[first,{id:'other',text:'另一条',status:'queued'}]);
});

test('queue reorder moves before a target or to the end without losing cards',()=>{
  const rows=['a','b','c','unknown'].map(id=>({id,status:id==='unknown'?'unknown':'queued'}));
  assert.deepEqual(Array.from(moveQueueRows(rows,'c','a'),row=>row.id),['c','a','b','unknown']);
  assert.deepEqual(Array.from(moveQueueRows(rows,'a',''),row=>row.id),['b','c','unknown','a']);
  assert.deepEqual(Array.from(moveQueueRows(rows,'b','c'),row=>row.id),rows.map(row=>row.id));
  assert.equal(moveQueueRows(rows,'b','c'),rows);
  assert.equal(moveQueueRows(rows,'a','missing'),rows);
});

test('queue pointer drag drops before the card under the pointer',()=>{
  const {context}=queueHelpers(),calls=[];
  const classList=()=>{const values=new Set();return{add(...names){names.forEach(name=>values.add(name));},remove(...names){names.forEach(name=>values.delete(name));}};};
  const cards=[['a',0,50],['b',60,40],['c',110,50]].map(([id,top,height])=>({dataset:{queueId:id},classList:classList(),getBoundingClientRect:()=>({top,height})}));
  const root={querySelectorAll:selector=>selector==='.queue-card[data-queue-id]'?cards:[]};
  const handle={};
  context.$=selector=>{assert.equal(selector,'queued');return root;};
  context.reorderQueuedMessage=(...args)=>calls.push(args);
  context.bindQueueDrag(handle,cards[1],{id:'b'},'thread','local');
  handle.onpointerdown({button:0,pointerId:1,preventDefault(){}});
  handle.onpointermove({clientY:20,preventDefault(){}});
  handle.onpointerup({type:'pointerup',pointerId:1});
  assert.deepEqual(calls,[['b','a','thread','local']]);
});

test('queue reorder sends the gateway contract and rolls back failed optimistic updates',async()=>{
  const {context}=queueHelpers(),rows=['a','b','c'].map(id=>({id,status:'queued'}));
  const calls=[];
  Object.assign(context,{state:{submissions:rows},currentId:'thread',currentHost:'local',sessionUrl:()=>'/queue',renderQueue:()=>{},t:text=>text,toast:()=>{}});
  context.api=async(path,body)=>{calls.push({path,body});return{status:'queued'};};
  await context.reorderQueuedMessage('c','a','thread','local');
  assert.equal(calls.length,1);assert.equal(calls[0].path,'/queue');
  assert.deepEqual({...calls[0].body},{id:'c',action:'reorder',beforeId:'a'});
  assert.deepEqual(Array.from(context.state.submissions,row=>row.id),['c','a','b']);
  const previous=context.state.submissions;
  context.api=async()=>{throw new Error('网络失败');};
  await context.reorderQueuedMessage('b','','thread','local');
  assert.equal(context.state.submissions,previous);
});

test('queue reorder survives stale snapshots until the gateway order catches up',async()=>{
  const {context}=queueHelpers(),rows=['a','b','c'].map(id=>({id,status:'queued'}));
  let release;
  Object.assign(context,{state:{submissions:rows},currentId:'thread',currentHost:'local',sessionUrl:()=>'/queue',renderQueue:()=>{},t:text=>text,toast:()=>{}});
  context.api=()=>new Promise(resolve=>{release=resolve;});
  const pending=context.reorderQueuedMessage('c','a','thread','local');
  await Promise.resolve();await Promise.resolve();
  const stale=context.applyPending({submissions:rows});
  assert.deepEqual(Array.from(stale.submissions,row=>row.id),['c','a','b']);
  release({status:'queued'});await pending;
  const staleAfterSuccess=context.applyPending({submissions:rows});
  assert.deepEqual(Array.from(staleAfterSuccess.submissions,row=>row.id),['c','a','b']);
  const confirmedRows=[rows[2],rows[0],rows[1]],confirmedView={submissions:confirmedRows};
  assert.equal(context.applyPending(confirmedView),confirmedView);
  assert.equal(context.pendingOrder(),null);
});

test('rapid queue reorders stay optimistic locally and sync in request order',async()=>{
  const {context}=queueHelpers(),rows=['a','b','c'].map(id=>({id,status:'queued'})),calls=[],releases=[];
  Object.assign(context,{state:{submissions:rows},currentId:'thread',currentHost:'local',sessionUrl:()=>'/queue',renderQueue:()=>{},t:text=>text,toast:()=>{}});
  context.api=(path,body)=>{calls.push(body);return new Promise(resolve=>releases.push(resolve));};
  const first=context.reorderQueuedMessage('c','a','thread','local');
  await Promise.resolve();await Promise.resolve();
  assert.equal(calls.length,1);
  const second=context.reorderQueuedMessage('a','','thread','local');
  assert.deepEqual(Array.from(context.state.submissions,row=>row.id),['c','b','a']);
  await Promise.resolve();await Promise.resolve();
  assert.equal(calls.length,1);
  releases[0]({status:'queued'});await first;
  await Promise.resolve();await Promise.resolve();
  assert.equal(calls.length,2);
  assert.deepEqual({...calls[1]},{id:'a',action:'reorder',beforeId:''});
  releases[1]({status:'queued'});await second;
  assert.deepEqual(Array.from(context.state.submissions,row=>row.id),['c','b','a']);
});

test('failed rapid reorders roll back to the last real server order',async()=>{
  const {context}=queueHelpers(),rows=['a','b','c'].map(id=>({id,status:'queued'})),calls=[],rejections=[];
  Object.assign(context,{state:{submissions:rows},currentId:'thread',currentHost:'local',sessionUrl:()=>'/queue',renderQueue:()=>{},t:text=>text,toast:()=>{}});
  context.api=(path,body)=>{calls.push(body);return new Promise((resolve,reject)=>rejections.push(reject));};
  const first=context.reorderQueuedMessage('c','a','thread','local');
  await Promise.resolve();await Promise.resolve();
  const second=context.reorderQueuedMessage('a','','thread','local');
  rejections[0](new Error('第一次失败'));await first;
  await Promise.resolve();await Promise.resolve();
  assert.equal(calls.length,2);
  rejections[1](new Error('第二次失败'));await second;
  assert.equal(context.state.submissions,rows);
  assert.equal(context.pendingOrder(),null);
});

test('queue reorder rollback uses the latest server snapshot and keeps new rows',async()=>{
  const {context}=queueHelpers(),rows=['a','b','c'].map(id=>({id,status:'queued'}));
  let reject;
  Object.assign(context,{state:{submissions:rows},currentId:'thread',currentHost:'local',sessionUrl:()=>'/queue',renderQueue:()=>{},t:text=>text,toast:()=>{}});
  context.api=()=>new Promise((resolve,rejectRequest)=>{reject=rejectRequest;});
  const pending=context.reorderQueuedMessage('c','a','thread','local');
  await Promise.resolve();await Promise.resolve();
  const serverRows=[...rows,{id:'d',status:'queued'}];
  const optimistic=context.applyPending({submissions:serverRows});
  assert.deepEqual(Array.from(optimistic.submissions,row=>row.id),['c','a','b','d']);
  reject(new Error('网络失败'));await pending;
  assert.equal(context.state.submissions,serverRows);
  assert.equal(context.pendingOrder(),null);
});

test('queue editor has a visible close icon',()=>{
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf("dialog.innerHTML='<div class=\"picker-head\""),end=source.indexOf("';",start);
  assert.ok(start>=0&&end>start);
  assert.match(source.slice(start,end),/aria-label="关闭">×<\/button>/);
});

test('queue drag also offers keyboard reordering',()=>{
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  assert.match(source,/ArrowUp/);
  assert.match(source,/ArrowDown/);
});

test('unknown delivery keeps protocol text collapsed behind a details disclosure',()=>{
  const{context,renderQueue}=queueHelpers(),queued={children:[],append(node){this.children.push(node);},replaceChildren(...nodes){this.children=nodes;}};
  const element=(tag,className,text)=>({tagName:tag.toUpperCase(),className,textContent:text||'',children:[],append(...nodes){this.children.push(...nodes);}});
  Object.assign(context,{
    $:selector=>{assert.equal(selector,'queued');return queued;},
    el:element,t:value=>value,currentId:'thread',currentHost:'local',renderQueueAttachments:()=>null,
    bindQueueDrag(){},sessionUrl:()=>'',api:async()=>({}),toast(){},openQueueEditor(){},ignoreSubmission:async()=>({}),
  });
  renderQueue([{id:'uncertain',status:'unknown',text:'[Team review] internal review protocol'}]);
  const card=queued.children[0],details=card.children.find(node=>node.tagName==='DETAILS');
  assert.ok(details);
  assert.equal(details.children[0].tagName,'SUMMARY');
  assert.equal(details.children[0].textContent,'查看原文');
  assert.equal(details.children[1].textContent,'[Team review] internal review protocol');
  assert.equal(card.children.some(node=>node.textContent==='[Team review] internal review protocol'),false);
});


test('queued messages keep the immediate-send gateway action',async()=>{
  const{context,renderQueue}=queueHelpers(),calls=[];
  const queued={children:[],append(node){this.children.push(node);},replaceChildren(...nodes){this.children=nodes;}};
  const element=(tag,className,text)=>({tagName:tag.toUpperCase(),className,textContent:text||'',dataset:{},children:[],setAttribute(){},append(...nodes){this.children.push(...nodes);}});
  Object.assign(context,{
    $:selector=>{assert.equal(selector,'queued');return queued;},
    el:element,t:value=>value,currentId:'thread',currentHost:'local',state:{submissions:[]},renderQueueAttachments:()=>null,
    bindQueueDrag(){},sessionUrl:()=>'/queue',api:async(path,body)=>{calls.push({path,body});return{status:'accepted'};},toast(){},openQueueEditor(){},
  });
  renderQueue([{id:'queued-1',status:'queued',text:'立即处理',editable:false}]);
  const actions=queued.children[0].children.find(node=>node.className==='queue-actions');
  assert.equal(actions.children[0].textContent,'立即发送');
  await actions.children[0].onclick();
  assert.equal(calls.length,1);
  assert.equal(calls[0].path,'/queue');
  assert.equal(calls[0].body.id,'queued-1');
  assert.equal(calls[0].body.action,'send');
});
