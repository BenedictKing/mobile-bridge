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
    constructor({url}){this.url=url;this.abort=new AbortController();}
    setViewportLocked(){}
    dispose(){this.abort.abort();}
    start(){return historyRead(this.url('changes'));}
  }
  const routes=[];
  const context={
    openChatRequest:0,currentId:null,currentHost:'local',currentTeam:null,state:null,teamDashboardPage:null,
    teamCanvas:null,chatTimeline:null,teamRunScreen:null,openRunRoute:'',modelRequest:0,skillRequest:0,notificationRequest:0,
    notificationState:{available:false,watching:false,notifyOnCompletion:false},
    teamChat:{teamForSession:id=>id==='team-session'?{id:'team'}:null,teamById:id=>id==='team'?{id:'team'}:null,open:noop},
    BridgeLazy:{load:()=>Promise.resolve()},chatRunSource:()=>null,syncChatRunSource:noop,syncWorkspaceNavigation:noop,
    teamQueueModeEnabled:()=>false,$:()=>node,document:{querySelectorAll:()=>sessionRows},messageActions:{pending:false,dialog:node},
    saveDraft:noop,closeSkillSuggestions:noop,renderAPKDownload:noop,syncDocumentTitle:noop,
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
