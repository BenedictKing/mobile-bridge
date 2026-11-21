'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),fs=require('node:fs');
class Source{constructor(url){this.url=url;this.handlers={};}addEventListener(kind,fn){this.handlers[kind]=fn;}close(){this.closed=true;}emit(kind,data,id=''){this.handlers[kind]?.({data:JSON.stringify(data),lastEventId:id});}}
async function fixture(callbacks={}){
  const sources=[],cursor={epoch:'e',after:1},status=[];
  const context={Promise,URLSearchParams,AbortController,setTimeout,clearTimeout,EventSource:Source,BridgeConnection:{request:async()=>({ok:true,json:async()=>({authenticated:true,transport:'sse'})}),eventSource:url=>{const s=new Source(url);sources.push(s);return s;}}};
  vm.createContext(context);vm.runInContext(fs.readFileSync('web/transport.js','utf8'),context);
  const transport=context.BridgeTransport,stop=transport.subscribe(action=>'/'+action,{cursor:()=>cursor,onStatus:s=>status.push(s),onChanges:page=>{cursor.after=page.sequence;},...callbacks});
  await new Promise(setImmediate);return{transport,stop,sources,cursor,status};
}
test('ready is distinct from applied synchronization and reconnect uses the applied cursor',async()=>{
  const f=await fixture({onDelta:()=>{}}),source=f.sources[0];
  source.emit('ready',{epoch:'e',sequence:2});source.emit('delta',{epoch:'e',sequence:2},'e.2');source.emit('synchronized',{epoch:'e',sequence:2});
  await new Promise(setImmediate);assert.ok(f.status.includes('synchronizing'));assert.ok(!f.status.includes('live'));
  f.transport.wake();assert.match(f.sources[1].url,/since=e.1/);f.stop();
});
test('retired SSE callbacks cannot apply frames or close the new connection',async()=>{
  const f=await fixture(),old=f.sources[0];f.transport.wake();const current=f.sources[1];
  old.emit('changes',{epoch:'old',sequence:999},'old.999');old.onerror();
  assert.equal(f.cursor.after,1);assert.ok(!current.closed);
  current.emit('changes',{epoch:'e',sequence:2},'e.2');current.emit('synchronized',{epoch:'e',sequence:2});
  await new Promise(setImmediate);assert.equal(f.cursor.after,2);assert.ok(f.status.includes('live'));f.stop();
});
