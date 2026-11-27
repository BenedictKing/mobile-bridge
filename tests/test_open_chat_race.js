'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

function loadOpenChat({historyRead=()=>Promise.resolve(),sessionRows=[]}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const invalidateStart=source.indexOf('function invalidatePendingChatOpen()');
  const invalidateEnd=source.indexOf('\nfunction closeDetailView',invalidateStart);
  const start=source.indexOf('async function openChat('),end=source.indexOf('\nlet permissionState=',start);
  const node=new Proxy({value:'',hidden:false,classList:{add(){},remove(){},contains(){return false;}},replaceChildren(){},removeAttribute(){},close(){},dataset:{}},{get:(target,key)=>key in target?target[key]:()=>{}});
  const noop=()=>{};
  class Timeline{
    constructor(options){this.options=options;this.url=options.url;this.abort=new AbortController();this.nodes=new Map();this.loads=[];this.restores=[];}
    setViewportLocked(){}
    dispose(){this.abort.abort();}
    start(){return historyRead(this.url('changes'));}
    async loadUntilKey(key){this.loads.push(key);return 0;}
    restore(anchor,following){this.restores.push([anchor,following]);}
  }
  const routes=[];
  const context={
    openChatRequest:0,currentId:null,currentHost:'local',currentTeam:null,state:null,teamDashboardPage:null,
    teamCanvas:null,chatTimeline:null,teamRunScreen:null,openRunRoute:'',modelRequest:0,skillRequest:0,notificationRequest:0,
    notificationState:{available:false,watching:false,notifyOnCompletion:false},
    teamChat:{teamForSession:id=>id==='team-session'?{id:'team'}:null,teamById:id=>id==='team'?{id:'team'}:null,open:noop},
    BridgeLazy:{load:()=>Promise.resolve()},chatRunSource:()=>null,syncChatRunSource:noop,syncWorkspaceNavigation:noop,
    teamQueueModeEnabled:()=>false,$:()=>node,document:{querySelectorAll:()=>sessionRows},messageActions:{pending:false,dialog:node},
    saveDraft:noop,closeSkillSuggestions:noop,renderAPKDownload:noop,syncDocumentTitle:noop,rememberChatPosition:noop,
    loadComposerDraft:()=>null,chatReadPositions:new Map(),chatOpenPending:null,setChatStatus:noop,syncChatStatus:noop,
    workModes:{open:noop},attachments:{open:noop},renderPermissionState:noop,renderCompactState:noop,renderNotificationState:noop,
    sessionStorage:{getItem:()=>null},renderSkillPills:noop,submittedRequestIds:new Set(),selectionQuote:null,
    t:value=>value,syncSendButton:noop,showDetailRoute:route=>routes.push(route),ChatTimeline:Timeline,api:()=>Promise.resolve(),
    sessionUrl:(id,action)=>id+'/'+action,renderState:noop,renderMarkdown:noop,renderDetailArtifact:noop,
    loadNotificationState:noop,loadPermissionSettings:noop,toast:noop,window:{},compactChat:{busy:false},
    teamSubmissionFields:()=>({}),chatKey:(id,host)=>host+'|'+id,fastModeControl:{reset:noop},startCatalog:noop,
  };
  vm.createContext(context);
  vm.runInContext(source.slice(invalidateStart,invalidateEnd)+'\n'+source.slice(start,end),context);
  return {context,node,routes,sessionRows};
}

function sessionRow(dataset){
  const classes=new Set();
  return {dataset,classes,classList:{toggle(name,selected){if(selected)classes.add(name);else classes.delete(name);}}};
}

test('opening a team selects its project entry and leaving clears it',async()=>{
  const teamEntry=sessionRow({teamId:'team'}),chatEntry=sessionRow({id:'regular-chat',host:'local'});
  const {context}=loadOpenChat({sessionRows:[teamEntry,chatEntry]});
  context.ensureTeamCanvas=()=>Promise.resolve();

  await context.openChat('team-session','local','team');
  assert.equal(teamEntry.classes.has('selected'),true);
  assert.equal(chatEntry.classes.has('selected'),false);

  await context.openChat('regular-chat','local');
  assert.equal(teamEntry.classes.has('selected'),false);
  assert.equal(chatEntry.classes.has('selected'),true);
});

test('opening Codex history does not activate an execution writer',async()=>{
  const {context}=loadOpenChat();
  const calls=[];
  context.api=(...args)=>{calls.push(args);return Promise.resolve();};
  await context.openChat('saved-codex');
  assert.equal(calls.some(([url])=>url.endsWith('/reconnect')),false);
});

test('a late team canvas load cannot override a newer chat selection',async()=>{
  const {context}=loadOpenChat();
  let releaseCanvas,markCanvasStarted;
  const canvas=new Promise(resolve=>{releaseCanvas=resolve;});
  const started=new Promise(resolve=>{markCanvasStarted=resolve;});
  context.ensureTeamCanvas=()=>{markCanvasStarted();return canvas;};

  const oldOpen=context.openChat('team-session');
  await started;
  await context.openChat('newer-chat');
  releaseCanvas();

  assert.equal(await oldOpen,false);
  assert.equal(context.currentId,'newer-chat');
});

test('leaving the chat view invalidates an unfinished chat open',async()=>{
  const {context}=loadOpenChat();
  let releaseCanvas,markCanvasStarted;
  const canvas=new Promise(resolve=>{releaseCanvas=resolve;});
  const started=new Promise(resolve=>{markCanvasStarted=resolve;});
  context.ensureTeamCanvas=()=>{markCanvasStarted();return canvas;};

  const oldOpen=context.openChat('team-session');
  await started;
  context.invalidatePendingChatOpen();
  releaseCanvas();

  assert.equal(await oldOpen,false);
  assert.equal(context.currentId,null);
});

test('a stale team open cannot publish its route after history finishes',async()=>{
  let releaseHistory,markHistoryStarted;
  const history=new Promise(resolve=>{releaseHistory=resolve;});
  const started=new Promise(resolve=>{markHistoryStarted=resolve;});
  const {context,routes}=loadOpenChat({historyRead:url=>{
    if(url.startsWith('team-session/')){markHistoryStarted();return history;}
    return Promise.resolve();
  }});
  context.ensureTeamCanvas=()=>Promise.resolve();
  let teamRoute='';
  const oldNavigation=(async()=>{
    if(await context.openChat('team-session','local','team'))teamRoute='#team=team';
  })();
  await started;
  await context.openChat('newer-chat');
  releaseHistory();

  assert.equal(await oldNavigation,undefined);
  assert.equal(teamRoute,'');
  assert.equal(context.currentId,'newer-chat');
  assert.equal(routes.at(-1),'#newer-chat~local');
});

test('reopening the visible same chat reuses its live timeline',async()=>{
  const {context}=loadOpenChat(),Timeline=context.ChatTimeline;
  context.currentId='chat';context.currentHost='local';context.state={};context.chatTimeline=new Timeline({url:()=>'chat/changes'});
  context.$('app').classList.contains=()=>true;context.$('chat').hidden=false;
  let loads=0,created=0;context.BridgeLazy.load=()=>{loads++;return Promise.resolve();};
  const Original=context.ChatTimeline;context.ChatTimeline=class extends Original{constructor(options){super(options);created++;}};
  const timeline=context.chatTimeline;
  assert.equal(await context.openChat('chat','local'),true);
  assert.equal(context.chatTimeline,timeline);assert.equal(loads,0);assert.equal(created,0);assert.equal(timeline.abort.signal.aborted,false);
});

test('an explicit Run turn still replaces an otherwise reusable timeline',async()=>{
  const {context}=loadOpenChat(),Timeline=context.ChatTimeline;
  context.currentId='chat';context.currentHost='local';context.state={};context.chatTimeline=new Timeline({url:()=>'chat/changes'});
  context.$('app').classList.contains=()=>true;context.$('chat').hidden=false;context.chatRunSource=()=>({turnId:'turn-2'});
  const Original=context.ChatTimeline;context.ChatTimeline=class extends Original{};
  const old=context.chatTimeline;
  await context.openChat('chat','local');
  assert.notEqual(context.chatTimeline,old);assert.equal(old.abort.signal.aborted,true);assert.equal(context.chatTimeline.options.initialTurnId,'turn-2');
});

test('an explicit refresh replaces a visible timeline even without a new turn',async()=>{
  const {context}=loadOpenChat(),Timeline=context.ChatTimeline;
  context.currentId='chat';context.currentHost='local';context.state={};context.chatTimeline=new Timeline({url:()=>'chat/changes'});
  context.$('app').classList.contains=()=>true;context.$('chat').hidden=false;
  const old=context.chatTimeline;
  await context.openChat('chat','local','',true);
  assert.notEqual(context.chatTimeline,old);assert.equal(old.abort.signal.aborted,true);
});

test('returning to a chat restores its saved anchor through bounded history loading',async()=>{
  const {context}=loadOpenChat();
  context.chatReadPositions.set('local|chat',{anchor:{key:'older-message',offset:18},following:false});
  await context.openChat('chat','local');
  assert.deepEqual(context.chatTimeline.loads,['older-message']);
  assert.deepEqual(context.chatTimeline.restores,[[{key:'older-message',offset:18},false]]);
});
