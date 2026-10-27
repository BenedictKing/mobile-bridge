'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {BridgeConnection}=require('../web/connection.js');

const origin='https://lan.bridge.example:8788';
const json=(value,status=200)=>new Response(JSON.stringify(value),{status,headers:{'Content-Type':'application/json'}});

function bootstrap(instance='instance'){
  const calls=[];
  globalThis.fetch=async(path,options={})=>{
    calls.push({path,options});
    if(path==='/api/direct/grant')return json({direct:origin,token:'token'});
    if(path===origin+'/api/direct/exchange')return json({csrf:'direct-csrf'});
    if(path===origin+'/api/auth')return json({authenticated:true,instanceId:instance});
    throw Error('unexpected '+path);
  };
  BridgeConnection.configure({direct:origin,instanceId:'instance'});
  BridgeConnection.authenticated('public-csrf');
  return BridgeConnection.probe().then(()=>calls);
}

test.afterEach(()=>{BridgeConnection.reset();delete globalThis.fetch;});

test('direct session is verified before API traffic switches',async()=>{
  const calls=await bootstrap();
  assert.equal(BridgeConnection.active,true);
  globalThis.fetch=async(path,options)=>{calls.push({path,options});return json({ok:true});};
  await BridgeConnection.request('/api/sessions');
  await BridgeConnection.request('/api/sessions',{method:'POST',body:'{}'});
  assert.equal(calls.at(-2).path,origin+'/api/sessions');
  assert.equal(calls.at(-2).options.credentials,'include');
  assert.equal(calls.at(-1).options.headers.get('X-CSRF-Token'),'direct-csrf');
});

test('instance mismatch keeps public transport',async()=>{
  await bootstrap('other-instance');
  assert.equal(BridgeConnection.active,false);
});

test('direct read failure falls back and later writes use public once',async()=>{
  await bootstrap();
  const calls=[];
  globalThis.fetch=async(path,options={})=>{
    calls.push({path,options});
    if(path.startsWith(origin))throw new TypeError('offline');
    return json({ok:true});
  };
  const response=await BridgeConnection.request('/api/sessions');
  assert.equal(response.status,200);
  assert.equal(BridgeConnection.active,false);
  assert.deepEqual(calls.map(call=>call.path),[origin+'/api/sessions','/api/sessions']);
  calls.length=0;
  await BridgeConnection.request('/api/sessions',{method:'POST',body:'{}'});
  assert.equal(calls.length,1);
  assert.equal(calls[0].path,'/api/sessions');
});

test('failed direct write is not retried on public transport',async()=>{
  await bootstrap();
  const calls=[];
  globalThis.fetch=async(path,options={})=>{calls.push({path,options});throw new TypeError('offline');};
  await assert.rejects(BridgeConnection.request('/api/sessions',{method:'POST',body:'{}'}),error=>error.directFallback===true);
  assert.equal(calls.length,1);
  assert.equal(BridgeConnection.active,false);
});

test('aborted direct request does not deactivate or retry',async()=>{
  await bootstrap();
  const controller=new AbortController();controller.abort();
  const calls=[];
  globalThis.fetch=async(path,options)=>{calls.push(path);throw new DOMException('aborted','AbortError');};
  await assert.rejects(BridgeConnection.request('/api/sessions',{signal:controller.signal}),{name:'AbortError'});
  assert.equal(BridgeConnection.active,true);
  assert.equal(calls.length,1);
});
