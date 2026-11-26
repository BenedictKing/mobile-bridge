'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function loadRecreate(overrides={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('let staleRecreating=null;'),end=source.indexOf('return staleRecreating;\n}',start)+'return staleRecreating;\n}'.length;
  assert.ok(start>0&&end>start);
  const storage=new Map(),calls=[],toasts=[],opened=[];
  const context={
    t:value=>value,uuid:()=>'req-1',currentId:'old',currentHost:'local',currentTeam:null,listRevision:'r1',
    sessionUrl:(id,action)=>'/api/sessions/'+id+'/'+action,
    chatKey:(id,host)=>host+'|'+id,
    sessionStorage:{getItem:key=>storage.get(key)??null,setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)},
    saveDraft:()=>storage.set('draft:local|'+context.currentId,'草稿'),
    attachments:{ids:()=>[]},
    api:async(path,body)=>{calls.push([path,body]);return path==='/api/sessions'?{id:'new',host:'local'}:{};},
    loadList:async()=>{},
    openChat:async(id,host)=>{opened.push([id,host]);context.currentId=id;},
    toast:message=>toasts.push(message),
    ...overrides,
  };
  vm.runInNewContext(source.slice(start,end),context);
  return {context,storage,calls,toasts,opened};
}

test('stale empty chat is recreated in the same agent and cwd with its draft',async()=>{
  const {context,storage,calls,toasts,opened}=loadRecreate();
  await context.recreateStaleChat('old','local',{agent:'claude',cwd:'/w',title:'旧标题'});
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])),['/api/sessions',{id:'req-1',agent:'claude',cwd:'/w',title:'旧标题'}]);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[1])),['/api/sessions/old/delete',{}]);
  assert.equal(storage.get('draft:local|new'),'草稿');
  assert.deepEqual(opened,[['new','local']]);
  assert.deepEqual(toasts,['原空聊天已失效，已重新创建']);
});

test('concurrent stale signals create only one replacement',async()=>{
  const {context,calls}=loadRecreate();
  const info={agent:'claude',cwd:'/w',title:''};
  await Promise.all([context.recreateStaleChat('old','local',info),context.recreateStaleChat('old','local',info)]);
  assert.equal(calls.filter(([path])=>path==='/api/sessions').length,1);
});

test('team, remote or switched-away chats are not recreated',async()=>{
  const info={agent:'claude',cwd:'/w'};
  for(const [overrides,id,host] of [[{currentTeam:{id:'team'}},'old','local'],[{currentHost:'mac'},'old','mac'],[{},'other','local']]){
    const {context,calls}=loadRecreate(overrides);
    await context.recreateStaleChat(id,host,info);
    assert.equal(calls.length,0);
  }
});

test('creation failure keeps the old chat and reports the error',async()=>{
  const {context,calls,toasts,opened}=loadRecreate({api:async(path)=>{calls.push([path]);throw Error('创建失败');}});
  await context.recreateStaleChat('old','local',{agent:'claude',cwd:'/w'});
  assert.deepEqual(calls,[['/api/sessions']]);
  assert.deepEqual(opened,[]);
  assert.deepEqual(toasts,['创建失败']);
});
