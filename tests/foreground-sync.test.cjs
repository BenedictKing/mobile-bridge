'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');

// -- transport wake: foreground forces a resync without waiting on backoff ---

function transportHarness(){
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
  let behavior='auth-sse';
  const authBody=(authenticated=true,transport='sse')=>({authenticated,csrf:'x',transport});
  const response=(body,status=200)=>({ok:status===200,status,json:async()=>body});
  const fetch=async(url)=>{
    if(url.startsWith('/api/auth')){
      if(behavior==='auth-poll')return response(authBody(true,'poll'));
      return response(authBody(true,'sse'));
    }
    calls.push({url});
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
  const subscribe=()=>{
    const seen={changes:[],resets:[],statuses:[]};
    const clone=value=>JSON.parse(JSON.stringify(value));
    const stop=transport.subscribe(action=>'/api/sessions/x/'+action+'?host=local',{
      onChanges:page=>seen.changes.push(clone(page)),onReset:page=>seen.resets.push(clone(page)),
      onStatus:text=>seen.statuses.push(text),
      cursor:()=>({after:7,epoch:'e1',start:'epoch.k1'})});
    return {stop,seen};
  };
  return {transport,sources,calls,pending,fire,settle,subscribe,set:value=>behavior=value};
}

test('wake short-circuits the backoff ladder and resets the failure count',async()=>{
  const h=transportHarness(),{stop}=h.subscribe();
  await h.settle();
  h.sources[0].fail();await h.settle();
  assert.equal(h.pending().length,1);
  assert.equal(h.pending()[0].ms,1000);
  h.transport.wake();await h.settle();
  assert.equal(h.sources.length,2,'wake resubscribes immediately');
  assert.equal(h.sources[0].closed,true);
  assert.equal(h.pending().length,0,'the backoff timer must be cleared');
  h.sources[1].emit('ready',{data:'{}'});
  h.sources[1].fail();await h.settle();
  assert.equal(h.pending()[0].ms,1000,'wake resets the failure count');
  stop();
});

test('wake replaces a healthy sse socket and replays from the last cursor',async()=>{
  const h=transportHarness(),{stop}=h.subscribe();
  await h.settle();
  h.sources[0].emit('changes',{lastEventId:'e1.8',data:JSON.stringify({})});
  h.transport.wake();await h.settle();
  assert.equal(h.sources[0].closed,true);
  assert.equal(h.sources[1].target,'/api/sessions/x/events?host=local&incremental=1&since=e1.8');
  stop();
});

test('wake never duplicates a long-poll loop and a stopped channel stays dead',async()=>{
  const h=transportHarness();h.set('auth-poll');
  const {stop}=h.subscribe();
  await h.settle();
  assert.equal(h.calls.length,1);
  h.transport.wake();await h.settle();
  // In-flight or pacing poll: wake must not start a second loop.
  assert.equal(h.calls.length,1);
  stop();
  h.transport.wake();await h.settle();
  assert.equal(h.calls.length,1,'a stopped channel must not resubscribe');
  assert.equal(h.transport.channels.size,0,'stop unregisters the wake handle');
});

// -- app.js: foreground refresh, stale responses, write-state preservation ---

async function appFixture(){
  const nodes=new Map(),listeners={},writes=[];
  const wakeCalls=[];
  const timelineOptions=[];
  const permissionQueue=[];
  let permissionGateActive=false;
  const node=(tag='')=>({tagName:tag.toUpperCase(),children:[],value:'',checked:false,disabled:false,hidden:false,open:false,textContent:'',title:'',dataset:{},options:[],style:{},scrollTop:0,scrollHeight:0,clientHeight:0,parentElement:{},
    classList:{add(){},remove(){},toggle(){},contains:()=>true},addEventListener(){},setAttribute(){},replaceChildren(...values){this.children=values;},append(...values){this.children.push(...values);},querySelectorAll(){return [];},querySelector(){return {disabled:false};},
    showModal(){this.open=true;},close(){this.open=false;},dispatchEvent(){}});
  const html=fs.readFileSync(path.join(__dirname,'../web/index.html'),'utf8');
  for(const match of html.matchAll(/\bid="([^"]+)"/g))nodes.set(match[1],node());
  const storage=()=>({getItem(){return null;},setItem(){},removeItem(){}});
  const response=(data,status=200)=>({ok:status===200,status,json:async()=>data});
  const documentStub={hidden:false,documentElement:{dataset:{}},addEventListener:(name,fn)=>{(listeners[name]??=[]).push(fn);},
    getElementById:id=>nodes.get(id),querySelector:()=>null,querySelectorAll:()=>[],createElement:node};
  const BridgeTransport={wake:()=>wakeCalls.push(Date.now()),subscribe:()=>()=>{}};
  class ChatTimeline{
    constructor(options){Object.assign(this,options);this.abort=new AbortController();timelineOptions.push(options);}
    async start(){}
    dispose(){this.abort.abort();}
    relabel(){}
  }
  const fetch=async(url,options={})=>{
    if(url.includes('/respond?')){writes.push({url,body:JSON.parse(options.body)});return response({ok:true});}
    if(url==='/api/auth')return response({authenticated:false});
    if(url.includes('/api/activity'))return response({sessions:[],listRevision:null});
    if(url.includes('/reconnect'))return response({ok:true});
    if(url.includes('/notifications?'))return response({available:true,watching:false,notifyOnCompletion:false,requests:'inherit',completion:'inherit'});
    if(url.includes('/permissions?')){
      if(permissionGateActive)return new Promise(resolve=>permissionQueue.push(data=>resolve(response(data))));
      return response({supported:false,options:[],current:null});
    }
    if(url.includes('/api/sessions?q='))return response({sessions:[],unavailableHosts:[]});
    return response({});
  };
  const context=vm.createContext({document:documentStub,renderMarkdown(){},window:{addEventListener(){}},
    localStorage:storage(),sessionStorage:storage(),navigator:{language:'zh-CN'},
    location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},
    setTimeout(){return 0;},clearTimeout(){},setInterval(){return 0;},
    crypto:{getRandomValues:()=>new Uint8Array(16)},
    Option:class{constructor(label,value){this.label=label;this.value=value;}},
    BridgeTransport,ChatTimeline,fetch,EventSource:undefined,AbortController,URLSearchParams});
  for(const file of ['web/i18n.js','web/account.js','web/modes.js','web/attachments.js','web/activity.js','web/fast-mode.js','web/message-actions.js','web/app.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,'..',file),'utf8'),context);
  await new Promise(setImmediate);
  const run=code=>vm.runInContext(code,context);
  const open=async(id='11111111-1111-4111-8111-111111111111',host='local')=>{
    await run(`openChat(${JSON.stringify(id)},${JSON.stringify(host)})`);await new Promise(setImmediate);
  };
  return {nodes,listeners,writes,wakeCalls,timelineOptions,permissionQueue,run,open,document:documentStub,
    gatePermissions:()=>{permissionGateActive=true;}};
}

test('returning to the foreground refreshes immediately instead of waiting a cycle',async()=>{
  const ui=await appFixture();
  await ui.open();
  ui.wakeCalls.length=0;
  ui.listeners.visibilitychange[0]();
  await new Promise(setImmediate);
  assert.equal(ui.wakeCalls.length,1,'the live channel is woken right away');
  assert.equal(ui.document.hidden,false);
});

test('a hidden tab never triggers the foreground wake',async()=>{
  const ui=await appFixture();
  await ui.open();
  ui.document.hidden=true;
  ui.listeners.visibilitychange[0]();
  await new Promise(setImmediate);
  assert.equal(ui.wakeCalls.length,0);
  ui.document.hidden=false;
});

test('late permission responses cannot overwrite a newer answer',async()=>{
  const ui=await appFixture();
  ui.gatePermissions();
  await ui.open();  // openChat parks the first permissions read.
  const parked=ui.run('loadPermissionSettings()');
  const parkedLater=ui.run('loadPermissionSettings()');
  await new Promise(setImmediate);
  assert.equal(ui.permissionQueue.length,3);
  // Resolve newest first, then the older ones with different data.
  ui.permissionQueue[2]({supported:true,options:[{value:'b',name:'B'}],current:'b'});
  ui.permissionQueue[1]({supported:true,options:[{value:'a',name:'A'}],current:'a'});
  ui.permissionQueue[0]({supported:false,options:[],current:null});
  await Promise.allSettled([parked,parkedLater]);
  assert.equal(ui.nodes.get('permission-select').value,'b','only the newest response may render');
});

test('a read failure after a successful write keeps the confirmed state',async()=>{
  const ui=await appFixture();
  await ui.open();
  const view={id:'11111111-1111-4111-8111-111111111111',agent:'codex',host:'local',hostLabel:'此电脑',
    title:'Sync test',cwd:'/work',connected:true,status:'idle',loadingHistory:false,activating:false,connecting:false,
    historyComplete:true,capabilities:['steer'],turns:[],
    requests:[{id:7,method:'item/commandExecution/requestApproval',supported:true,
               params:{command:'ls',availableDecisions:['accept','decline']}}],
    submissions:[{id:'sub1',text:'排队消息',status:'queued',editable:true}]};
  ui.run(`renderState(${JSON.stringify(view)})`);
  const card=ui.nodes.get('approvals').children[0];
  assert.ok(card,'the approval card renders');
  assert.equal(ui.nodes.get('queued').children.length,1,'the queued message renders');
  const form=card.children.find(n=>n.tagName==='FORM');
  assert.ok(form,'the approval form renders');
  form.onsubmit({preventDefault(){}});
  // onsubmit 派发的 submit() 不返回 promise，用两个事件循环拍冲掉微任务链。
  await new Promise(setImmediate);await new Promise(setImmediate);
  assert.equal(ui.writes.length,1,'the decision was submitted once');
  assert.match(ui.writes[0].url,/\/respond\?/);
  assert.deepEqual(ui.writes[0].body,{requestId:7,response:{decision:'accept'}});
  const acknowledged=card.children.some(n=>typeof n.textContent==='string'&&n.textContent.includes('已发送，等待桌面确认…'));
  assert.ok(acknowledged,'the success note stays on the card');
  // Simulate the next read failing: the live channel only reports status.
  ui.timelineOptions[ui.timelineOptions.length-1].status('重新连接中');
  assert.equal(ui.nodes.get('approvals').children[0],card,'a failed read does not rebuild approvals');
  assert.ok(ui.nodes.get('approvals').children[0].children.some(n=>typeof n.textContent==='string'&&n.textContent.includes('已发送，等待桌面确认…')),'the acknowledged approval survives');
  assert.equal(ui.nodes.get('queued').children.length,1,'the queue state survives');
});
