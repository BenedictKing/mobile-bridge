'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function uniqueQueueRows(rows){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function uniqueQueueRows(rows){');
  const end=source.indexOf('function renderQueue(rows){',start);
  assert.ok(start>=0&&end>start,'queue dedupe helper not found');
  const context={};
  vm.runInNewContext(source.slice(start,end)+'\nthis.unique=uniqueQueueRows;',context);
  return context.unique(rows);
}

test('queue keeps only the first row for each submission id',()=>{
  const first={id:'same',text:'来自实时快照',status:'queued'};
  const rows=uniqueQueueRows([first,{id:'same',text:'本地追加',status:'queued'},{id:'other',text:'另一条',status:'queued'}]);
  assert.deepEqual(rows,[first,{id:'other',text:'另一条',status:'queued'}]);
});
