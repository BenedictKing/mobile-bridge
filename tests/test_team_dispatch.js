'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const {TeamChatController}=require('../web/team-chat.js');
const source=readFileSync(require.resolve('../web/app.js'),'utf8');
const teams=[
  {id:'other',name:'其他团队',projectCwd:'/other',leadId:'lead',members:[{id:'lead',sessionId:'other-lead'}]},
  {id:'project-team',name:'项目团队',projectCwd:'/work',defaultForProject:true,leadId:'lead',members:[{id:'lead',sessionId:'project-lead'}]},
];
class FakeNode{
  constructor(){this.value='';this.children=[];this.listeners={};this.disabled=false;this.hidden=false;this.open=false;this.dataset={};}
  replaceChildren(...children){this.children=children;this.value=children[0]?.value||'';}
  addEventListener(name,handler){this.listeners[name]=handler;}
  showModal(){this.open=true;}
  close(){this.open=false;this.listeners.close?.();}
  setAttribute(){}
}
function setup({text='检查登录流程',directory=teams,request,send,queue=false}={}){
  const nodes={},storage=new Map(),calls=[],toasts=[];let counter=0;
  const $=id=>nodes[id]||(nodes[id]=new FakeNode());
  $('message').value=text;storage.set('draft:local|source-chat',text);
  const teamChat=new TeamChatController({root:{}});
  const context={
    $,t:value=>value,currentId:'source-chat',currentHost:'local',currentTeam:null,sending:false,
    state:{cwd:'/work',project:'kimi:work'},teamChat,teamSubmissionFields:()=>({teamIssueRoutingVersion:'1',...(queue?{teamQueueVersion:'2'}:{})}),
    attachments:{ready:()=>true,ids:()=>[]},Option:class{constructor(text,value){this.textContent=text;this.value=value;}},
    sessionStorage:{getItem:key=>storage.get(key)||null,setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)},
    uuid:()=>`request-${++counter}`,chatKey:(id='source-chat',host='local')=>host+'|'+id,
    BridgeI18n:{locale:()=> 'zh'},renderList(){},renderTeamSidebarTeams(){},updateSkillSuggestions(){},
    window:{BridgePresentation:{resizeMessage(){}}},toast:value=>toasts.push(value),openTeamDashboard(){context.dashboardOpened=true;},
    teamRequest:async path=>request?request(path):{teams:directory},
    api:async(path,body)=>{calls.push({path,body});return send?send(path,body):{status:'accepted',inputId:body.id,teamId:'project-team'};},
  };
  const syncStart=source.indexOf('function syncSendButton(){'),syncEnd=source.indexOf("$('composer').onsubmit=",syncStart);
  const start=source.indexOf('let teamDispatchSource='),end=source.indexOf('function createTeamDashboardShell',start);
  assert.ok(syncStart>=0&&syncEnd>syncStart&&start>=0&&end>start);
  vm.runInNewContext(source.slice(syncStart,syncEnd)+'\n'+source.slice(start,end),context);
  return{context,nodes,storage,calls,toasts,open:()=>context.openTeamDispatch(),submit:()=>context.submitTeamDispatch({preventDefault(){}})};
}
test('dispatches the draft to the project team without sending to the source chat or navigating',async()=>{
  const {context,nodes,calls,storage,toasts,open,submit}=setup();
  await open();assert.equal(nodes['team-dispatch-team'].value,'project-team');
  await submit();assert.equal(calls.length,1);
  assert.equal(calls[0].path,'/api/teams/project-team/inputs');
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].body)),{id:'request-1',text:'检查登录流程',mode:'send',workMode:'default',uiLocale:'zh',teamIssueRoutingVersion:'1'});
  assert.equal(context.currentId,'source-chat');assert.equal(context.currentTeam,null);
  assert.equal(nodes.message.value,'');assert.equal(storage.has('draft:local|source-chat'),false);
  assert.equal(nodes['team-dispatch-dialog'].open,false);assert.deepEqual(toasts,['任务已交给团队：项目团队']);
});
test('failed delivery keeps the draft and reuses its ID after closing, switching teams and reopening',async()=>{
  const {nodes,calls,open,submit}=setup({send:async()=>{throw Error('连接中断');}});
  await open();await submit();assert.equal(nodes.message.value,'检查登录流程');
  assert.equal(nodes['team-dispatch-error'].textContent,'连接中断');
  nodes['team-dispatch-team'].value='other';await submit();
  nodes['team-dispatch-dialog'].close();await open();nodes['team-dispatch-team'].value='project-team';await submit();
  assert.equal(calls[0].body.id,calls[2].body.id);assert.notEqual(calls[0].body.id,calls[1].body.id);
});
test('double submit dispatches once and a newer source draft survives an in-flight response',async()=>{
  let resolve;
  const {nodes,calls,storage,open,submit}=setup({send:()=>new Promise(done=>resolve=done)});
  await open();const pending=submit();await submit();assert.equal(calls.length,1);
  nodes.message.value='补充的新任务';storage.set('draft:local|source-chat','补充的新任务');
  resolve({status:'accepted'});await pending;
  assert.equal(nodes.message.value,'补充的新任务');assert.equal(storage.get('draft:local|source-chat'),'补充的新任务');
});
test('editing the task in the dialog preserves the original conversation draft',async()=>{
  const {nodes,calls,open,submit}=setup();await open();nodes['team-dispatch-text'].value='只检查移动端';await submit();
  assert.equal(calls[0].body.text,'只检查移动端');assert.equal(nodes.message.value,'检查登录流程');
});
test('edited task drafts and selected teams survive closing and retrying after failure',async()=>{
  const fixture=setup({send:async()=>{throw Error('连接中断');}});await fixture.open();
  fixture.nodes['team-dispatch-text'].value='编辑后的派发任务';fixture.nodes['team-dispatch-team'].value='other';
  await fixture.submit();fixture.nodes['team-dispatch-dialog'].close();await fixture.open();
  assert.equal(fixture.nodes['team-dispatch-text'].value,'编辑后的派发任务');assert.equal(fixture.nodes['team-dispatch-team'].value,'other');
  await fixture.submit();assert.equal(fixture.calls[0].body.id,fixture.calls[1].body.id);
});
test('queue preferences use the team input endpoint version and remote source chats still dispatch locally',async()=>{
  const {context,calls,open,submit}=setup({queue:true});context.currentHost='ssh';await open();await submit();
  assert.equal(calls[0].path,'/api/teams/project-team/inputs');assert.equal(calls[0].body.queueVersion,'2');
  assert.equal(calls[0].body.teamQueueVersion,undefined);
});
test('no teams, missing leads, oversized tasks and unknown acceptance do not discard the draft',async()=>{
  const empty=setup({directory:[{id:'broken',members:[]}]});await empty.open();await empty.submit();
  assert.equal(empty.nodes['team-dispatch-submit'].disabled,true);assert.equal(empty.nodes['team-dispatch-manage'].hidden,false);
  assert.equal(empty.calls.length,0);assert.equal(empty.nodes.message.value,'检查登录流程');
  const large=setup({text:'a'.repeat(10001)});await large.open();await large.submit();assert.equal(large.calls.length,0);
  const unknown=setup({send:async()=>({status:'unknown'})});await unknown.open();await unknown.submit();
  assert.equal(unknown.nodes.message.value,'检查登录流程');assert.equal(unknown.nodes['team-dispatch-dialog'].open,true);
});
test('attachments are not silently lost and read-only/team chats have no dispatch entry',async()=>{
  const fixture=setup();fixture.context.attachments.ids=()=>['file-1'];await fixture.open();assert.equal(fixture.nodes['team-dispatch-dialog'].open,false);
  assert.match(fixture.toasts[0],/附件/);
  fixture.context.state.readOnly=true;fixture.context.syncSendButton();assert.equal(fixture.nodes['team-dispatch-button'].hidden,true);
  fixture.context.state.readOnly=false;fixture.context.currentTeam=teams[0];fixture.context.syncSendButton();assert.equal(fixture.nodes['team-dispatch-button'].hidden,true);
});
test('closing the picker before its directory request finishes cannot overwrite a newly opened picker',async()=>{
  const reads=[];const fixture=setup({request:()=>new Promise(resolve=>reads.push(resolve))});
  const first=fixture.open();fixture.nodes['team-dispatch-dialog'].close();const second=fixture.open();
  reads[0]({teams:[]});await first;reads[1]({teams});await second;
  assert.equal(fixture.nodes['team-dispatch-team'].value,'project-team');assert.equal(fixture.nodes['team-dispatch-manage'].hidden,true);
});
test('opening a team conversation keeps the chat tab while dashboard and Run pages use the team tab',()=>{
  const nodes={},$=id=>nodes[id]||(nodes[id]={hidden:true,classList:{toggle(name,value){this[name]=value;}},setAttribute(){},toggleAttribute(name,value){this[name]=value;}});
  const context={$,t:value=>value,currentTeam:teams[1],teamDashboardPage:{hidden:true},openRunRoute:'',renderTeamSidebarTeams(){},document:{querySelector:$}};
  const start=source.indexOf('function syncWorkspaceNavigation(){'),end=source.indexOf('function renderTeamSidebarTeams',start);
  vm.runInNewContext(source.slice(start,end),context);context.syncWorkspaceNavigation();
  assert.equal($('chats-button').classList.active,true);assert.equal($('#sessions').hidden,false);
  context.teamDashboardPage.hidden=false;context.syncWorkspaceNavigation();assert.equal($('teams-button').classList.active,true);
  context.teamDashboardPage.hidden=true;context.openRunRoute='run-1';$('team-run').hidden=false;
  context.syncWorkspaceNavigation();assert.equal($('teams-button').classList.active,true);
});
