'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const {LocalNotify}=require('../web/local-notify.js');
const {SessionActivity}=require('../web/activity.js');

function browserWin(permission='default'){
  const store=new Map();
  const listeners={};
  const win={
    isSecureContext:true,
    Notification:class{
      constructor(title,options){this.title=title;this.options=options;this.onclick=null;win.created.push(this);}
      close(){this.closed=true;}
      static permission=permission;
      static async requestPermission(){this.permission='granted';return 'granted';}
    },
    created:[],
    localStorage:{getItem:key=>store.get(key)||null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)},
    location:{hash:''},
    focus(){this.focused=true;},
    addEventListener:(name,handler)=>{(listeners[name]??=[]).push(handler);},
    removeEventListener:(name,handler)=>{listeners[name]=(listeners[name]||[]).filter(h=>h!==handler);},
    emit(name,detail){for(const h of [...(listeners[name]||[])])h({detail});},
  };
  return win;
}

test('browser notifications require secure context and a real API',()=>{
  const insecure=browserWin();insecure.isSecureContext=false;
  assert.equal(new LocalNotify({window:insecure}).supported,false);
  const noApi=browserWin();delete noApi.Notification;
  assert.equal(new LocalNotify({window:noApi}).supported,false);
  assert.equal(new LocalNotify({window:browserWin()}).supported,true);
});

test('enabling asks permission once and persists the choice',async()=>{
  const win=browserWin();
  const notify=new LocalNotify({window:win});
  assert.equal(notify.enabled,false);
  assert.equal(await notify.setEnabled(true),true);
  assert.equal(win.created.length,0);
  assert.equal(win.localStorage.getItem('local-notify'),'on');
  notify.notify({key:'k',title:'t',body:'b',host:'local',id:'1'});
  assert.equal(win.created.length,1);
  assert.equal(win.created[0].options.tag,'k');
  await notify.setEnabled(false);
  notify.notify({key:'k2',title:'t',body:'b',host:'local',id:'1'});
  assert.equal(win.created.length,1);
  assert.equal(win.localStorage.getItem('local-notify'),null);
});

test('denied permission keeps the switch off',async()=>{
  const win=browserWin();
  win.Notification.requestPermission=async()=>{win.Notification.permission='denied';return 'denied';};
  const notify=new LocalNotify({window:win});
  assert.equal(await notify.setEnabled(true),false);
  assert.equal(notify.enabled,false);
  assert.equal(notify.state().message,'系统已拒绝通知，请在浏览器或系统设置中允许');
  assert.equal(win.localStorage.getItem('local-notify'),null);
});

test('clicking a browser notification opens the chat route and focuses',async()=>{
  const win=browserWin('granted');
  const notify=new LocalNotify({window:win});
  await notify.setEnabled(true);
  notify.notify({key:'k',title:'t',body:'b',host:'local',id:'abc'});
  win.created[0].onclick();
  assert.equal(win.location.hash,'#abc~local');
  assert.equal(win.focused,true);
  assert.equal(win.created[0].closed,true);
});

function nativeWin(permission='default'){
  const win=browserWin();
  delete win.Notification;
  const store=new Map();
  const calls=[];
  win.MobileBridgeNative={
    notifications:()=>'supported',
    notificationPermission:()=>permission,
    setNotificationsEnabled:value=>{calls.push(['enabled',value]);permission='granted';win.emit('mobilebridge-notification-permission',{permission});},
    notify:payload=>calls.push(['notify',JSON.parse(payload)]),
    openChat:(host,id)=>calls.push(['open',host,id]),
  };
  win.calls=calls;
  return win;
}

test('android native bridge sends notifications only after grant',async()=>{
  const win=nativeWin();
  const notify=new LocalNotify({window:win});
  assert.equal(notify.supported,true);
  notify.notify({key:'k',title:'t',body:'b',host:'local',id:'1'});
  assert.equal(win.calls.filter(c=>c[0]==='notify').length,0);
  assert.equal(await notify.setEnabled(true),true);
  notify.notify({key:'k',title:'t',body:'b',host:'local',id:'1'});
  assert.deepEqual(win.calls.filter(c=>c[0]==='notify')[0][1],{key:'k',title:'t',body:'b',host:'local',id:'1'});
  notify.focus('1','local');
  assert.deepEqual(win.calls.at(-1),['open','local','1']);
  await notify.setEnabled(false);
  assert.deepEqual(win.calls.at(-1),['enabled',false]);
});

test('old android APK without the bridge reports unsupported',()=>{
  const win=browserWin();delete win.Notification;
  win.MobileBridgeNative={setTheme(){}};
  assert.equal(new LocalNotify({window:win}).supported,false);
});

test('android notification target survives a logged-out cold start',()=>{
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function consumeNativeChat(');
  const end=source.indexOf("window.addEventListener('mobilebridge-notification-permission'",start);
  const app={hidden:true},opened=[],listeners={};
  let consumed=0;
  const context={
    $:id=>id==='app'?app:null,
    window:{
      MobileBridgeNative:{consumePendingChat(){consumed++;return '{"host":"local","id":"c1"}';}},
      addEventListener:(name,handler)=>{listeners[name]=handler;},
    },
    openChat:(id,host)=>{opened.push({id,host});return Promise.resolve();},
    toast(){},
  };
  vm.runInNewContext(source.slice(start,end),context);
  listeners['mobilebridge-open-chat']({detail:{host:'local',id:'c1'}});
  assert.equal(consumed,0);
  assert.deepEqual(opened,[]);
  app.hidden=false;
  listeners['mobilebridge-open-chat']({detail:{host:'local',id:'c1'}});
  assert.equal(consumed,1);
  assert.deepEqual(opened,[{id:'c1',host:'local'}]);
});

function row(extra){return {host:'local',id:'c1',connected:true,status:'idle',notify:{requests:true,completion:true},...extra};}

test('activity edges notify once and skip the visible chat',()=>{
  const activity=new SessionActivity();
  assert.deepEqual(activity.localNotifications([row({status:'active'})],null,true),[]);
  const pending=activity.localNotifications([row({status:'active',pendingRequests:2})],null,true);
  assert.equal(pending.length,1);
  assert.equal(pending[0].kind,'requests');
  assert.deepEqual(activity.localNotifications([row({status:'active',pendingRequests:2})],null,true),[]);
  assert.deepEqual(activity.localNotifications([row({status:'active',pendingRequests:3})],'local|c1',true),[]);
  const done=activity.localNotifications([row({turnId:'t1',turnStatus:'completed',pendingRequests:0})],null,true);
  assert.equal(done.length,1);
  assert.equal(done[0].kind,'completed');
  assert.deepEqual(activity.localNotifications([row({turnId:'t1',turnStatus:'completed'})],null,true),[]);
  assert.deepEqual(activity.localNotifications([row({turnId:'t2',turnStatus:'completed'})],'local|c1',true),[]);
});

test('foreground transitions update the baseline without notifying later',()=>{
  const activity=new SessionActivity();
  activity.localNotifications([row({status:'active'})],null,true);
  assert.deepEqual(activity.localNotifications([row({pendingRequests:1})],null,false),[]);
  assert.deepEqual(activity.localNotifications([row({pendingRequests:1})],null,true),[]);
  activity.localNotifications([row({pendingRequests:0})],null,true);
  assert.equal(activity.localNotifications([row({pendingRequests:1})],null,true).length,1);
});

test('completion notifications respect the per-chat policy',()=>{
  const activity=new SessionActivity();
  activity.localNotifications([row({status:'active',notify:{requests:true,completion:false}})],null,true);
  assert.deepEqual(activity.localNotifications([row({turnId:'t1',turnStatus:'completed',notify:{requests:true,completion:false}})],null,true),[]);
  assert.deepEqual(activity.localNotifications([row({pendingRequests:1,notify:{requests:false,completion:true}})],null,true),[]);
});

test('clearing activity resets the notification baseline',()=>{
  const activity=new SessionActivity();
  activity.localNotifications([row({status:'active'})],null,true);
  activity.clear();
  assert.deepEqual(activity.localNotifications([row({pendingRequests:1})],null,true),[]);
});
