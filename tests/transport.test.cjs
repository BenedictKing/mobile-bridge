'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');

function harness(){
  const timers=[];let order=0;
  class FakeEventSource{
    constructor(target){this.target=target;this.listeners={};this.onerror=null;this.closed=false;sources.push(this);}
    addEventListener(name,fn){(this.listeners[name]??=[]).push(fn);}
    close(){this.closed=true;}
    emit(name,message={lastEventId:'',data:'{}'}){for(const fn of this.listeners[name]||[])fn(message);}
    fail(){this.onerror?.(new Error('network'));}
  }
  const sources=[];
  const calls=[];
  const fetchLog=[];
  let behavior='auth-sse';
  const authBody=(authenticated=true,transport='sse')=>({authenticated,csrf:'x',transport});
  const response=(body,status=200)=>({ok:status===200,status,json:async()=>body});
  const fetch=async(url,options)=>{
    fetchLog.push({url,options});
    if(url.startsWith('/api/auth')){
      if(behavior==='auth-down')throw Error('network');
      if(behavior==='auth-expired')return response(authBody(false));
      return response(authBody(true,behavior==='auth-poll'?'poll':'sse'));
    }
    calls.push({url,options});
    if(behavior==='changes-401')return response({},401);
    if(behavior==='changes-500')return response({},500);
    return response({epoch:'e1',sequence:9,rows:[{key:'k9'}],meta:{}});
  };
  const context=vm.createContext({fetch,EventSource:FakeEventSource,URLSearchParams,AbortController,
    setTimeout:(fn,ms)=>{const id={fn,ms:Number(ms)||0,cleared:false,order:++order};timers.push(id);return id;},
    clearTimeout:id=>{if(id)id.cleared=true;}});
  vm.runInContext(fs.readFileSync(path.join(__dirname,'../web/transport.js'),'utf8'),context);
  const transport=vm.runInContext('BridgeTransport',context);
  const pending=()=>timers.filter(t=>!t.cleared);
  const fire=()=>{const t=pending()[0];if(!t)throw Error('no timer scheduled');t.cleared=true;t.fn();};
  const settle=async()=>{await new Promise(resolve=>global.setTimeout(resolve,0));};
  return {transport,sources,fetchLog,calls,timers,pending,fire,settle,
    set:value=>behavior=value,
    subscribe:(overrides={})=>{const seen={changes:[],resets:[],statuses:[]};
      // vm-context objects carry a different prototype; clone for assertions.
      const clone=value=>JSON.parse(JSON.stringify(value));
      const stop=transport.subscribe(action=>'/api/sessions/x/'+action+'?host=local',{
        onChanges:page=>seen.changes.push(clone(page)),onReset:page=>seen.resets.push(clone(page)),
        onStatus:text=>seen.statuses.push(text),
        cursor:()=>({after:7,epoch:'e1',start:'epoch.k1',...overrides.cursor}),
        ...overrides.extra});
      return {stop,seen};}};
}

test('backoff doubles from one second and caps at thirty-two',()=>{
  const {transport}=harness();
  assert.equal(transport.backoff(1),1000);
  assert.equal(transport.backoff(2),2000);
  assert.equal(transport.backoff(5),16000);
  assert.equal(transport.backoff(6),32000);
  assert.equal(transport.backoff(40),32000);
});

test('sse channel subscribes with cursor and maps ready/changes/reset frames',async()=>{
  const h=harness(),{stop,seen}=h.subscribe();
  await h.settle();
  assert.equal(h.sources.length,1);
  assert.equal(h.sources[0].target,'/api/sessions/x/events?host=local&incremental=1&since=e1.7');
  h.sources[0].emit('ready',{data:JSON.stringify({epoch:'e1',sequence:7})});
  h.sources[0].emit('changes',{lastEventId:'e1.8',data:JSON.stringify({epoch:'e1',sequence:8,rows:[{key:'k8'}]})});
  h.sources[0].emit('reset',{lastEventId:'e1.9',data:JSON.stringify({epoch:'e1',sequence:9,rows:[{key:'k9'}],reset:true})});
  assert.deepEqual(seen.changes,[{epoch:'e1',sequence:8,rows:[{key:'k8'}]}]);
  assert.deepEqual(seen.resets,[{epoch:'e1',sequence:9,rows:[{key:'k9'}],reset:true}]);
  assert.deepEqual(seen.statuses,[]);
  // Reconnects resume from the last frame id, not from the initial cursor.
  h.sources[0].fail();
  await h.settle();
  assert.deepEqual(seen.statuses,['reconnecting']);
  h.fire();
  await h.settle();
  assert.equal(h.sources[0].closed,true);
  assert.equal(h.sources[1].target,'/api/sessions/x/events?host=local&incremental=1&since=e1.9');
  stop();
});

test('ready frame clears the failure count so backoff restarts at one second',async()=>{
  const h=harness(),{stop,seen}=h.subscribe();
  await h.settle();
  h.sources[0].fail();await h.settle();
  assert.equal(h.pending()[0].ms,1000);
  h.fire();await h.settle();
  h.sources[1].emit('ready',{data:'{}'});
  h.sources[1].fail();await h.settle();
  assert.equal(h.pending()[0].ms,1000,'ready must reset the backoff ladder');
  h.fire();await h.settle();
  h.sources[2].fail();await h.settle();
  assert.equal(h.pending()[0].ms,2000);
  stop();
});

test('poll channel reads the live cursor every cycle and stops on 401',async()=>{
  const h=harness();
  h.set('auth-poll');
  let cursor={after:7,epoch:'e1',start:'epoch.k1'};
  const seen={changes:[],resets:[],statuses:[]};
  const clone=value=>JSON.parse(JSON.stringify(value));
  const stop=h.transport.subscribe(action=>'/api/sessions/x/'+action+'?host=local',{
    onChanges:page=>seen.changes.push(clone(page)),onReset:page=>seen.resets.push(clone(page)),
    onStatus:text=>seen.statuses.push(text),cursor:()=>cursor});
  await h.settle();
  assert.equal(h.sources.length,0,'tunnel transport must not open an EventSource');
  assert.equal(h.calls[0].url,'/api/sessions/x/changes?host=local&after=7&epoch=e1&start=epoch.k1');
  assert.equal(h.calls[0].options.credentials,'same-origin');
  assert.deepEqual(seen.changes,[{epoch:'e1',sequence:9,rows:[{key:'k9'}],meta:{}}]);
  await new Promise(resolve=>global.setTimeout(resolve,0));  // 200ms pacing is a vm timer
  h.fire();
  await h.settle();
  cursor={after:9,epoch:'e1',start:'epoch.k3'};
  h.set('changes-401');
  await new Promise(resolve=>global.setTimeout(resolve,0));
  h.fire();
  await h.settle();
  assert.deepEqual(seen.statuses.slice(-1),['unauthorized']);
  assert.equal(h.pending().length,0,'401 must not schedule a retry');
  stop();
});

test('failed probes classify as reconnecting with growing backoff',async()=>{
  const h=harness();
  h.set('auth-down');
  const {stop,seen}=h.subscribe();
  await h.settle();
  assert.deepEqual(seen.statuses,['reconnecting']);
  const delays=[];
  for(let i=0;i<3;i++){delays.push(h.pending()[0].ms);h.fire();await h.settle();}
  assert.deepEqual(delays,[1000,2000,4000]);
  stop();
});

test('expired login and logout frames end the subscription without retries',async()=>{
  const h=harness();
  h.set('auth-expired');
  const expired=h.subscribe();
  await h.settle();
  assert.deepEqual(expired.seen.statuses,['unauthorized']);
  assert.equal(h.sources.length,0);
  expired.stop();
  h.set('auth-sse');
  const {stop,seen}=h.subscribe();
  await h.settle();
  h.sources[0].emit('logout',{data:'{}'});
  assert.deepEqual(seen.statuses,['unauthorized']);
  assert.equal(h.sources[0].closed,true);
  assert.equal(h.pending().length,0);
  stop();
});

test('unsubscribe is idempotent and cancels pending work',async()=>{
  const h=harness(),{stop}=h.subscribe();
  await h.settle();
  h.sources[0].fail();await h.settle();
  assert.equal(h.pending().length,1);
  stop();stop();
  assert.equal(h.pending().length,0);
  assert.equal(h.sources[0].closed,true);
});
