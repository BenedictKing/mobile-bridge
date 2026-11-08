'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function queueHelpers(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function uniqueQueueRows(rows){');
  const end=source.indexOf('function renderQueue(rows){',start);
  assert.ok(start>=0&&end>start,'queue helpers not found');
  const context={};
  vm.runInNewContext(source.slice(start,end)+'\nthis.unique=uniqueQueueRows;this.move=moveQueueRows;',context);
  return{uniqueQueueRows:context.unique,moveQueueRows:context.move,context};
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
