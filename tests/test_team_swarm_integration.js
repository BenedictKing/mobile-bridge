'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.style={};this.dataset={};this.className='';this.textContent='';this.listeners={};}
  append(...nodes){for(const node of nodes)node.parent=this;this.children.push(...nodes);}
  prepend(...nodes){for(const node of nodes)node.parent=this;this.children.unshift(...nodes);}
  replaceChildren(...nodes){this.children=[];this.append(...nodes);}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(name,listener){this.listeners[name]=listener;}
  querySelector(selector){return this.querySelectorAll(selector)[0]||null;}
  querySelectorAll(selector){return descendants(this).filter(node=>selector==='*'||selector.startsWith('.')?selector==='*'||node.className.split(/\s+/).includes(selector.slice(1)):node.tag===selector);}
  remove(){if(this.parent)this.parent.children=this.parent.children.filter(node=>node!==this);}
  insertBefore(node,target){node.parent=this;const index=this.children.indexOf(target);if(index<0)this.children.push(node);else this.children.splice(index,0,node);}
  showModal(){this.open=true;}
  close(){this.open=false;}
  get classList(){const node=this;return {add(value){node.className+=' '+value;},contains(value){return node.className.split(/\s+/).includes(value);},remove(){},toggle(){}};}
}
function descendants(node){return node.children.flatMap(child=>[child,...descendants(child)]);}
function text(node){return node.textContent+node.children.map(text).join('');}
global.document={createElement:tag=>new FakeNode(tag),createElementNS:(namespace,tag)=>new FakeNode(tag),createTextNode:value=>Object.assign(new FakeNode('#text'),{textContent:value}),body:new FakeNode('body'),querySelectorAll:()=>[]};
global.Option=class extends FakeNode{constructor(text,value){super('option');this.textContent=text;this.value=value;}};
global.localStorage={getItem:()=> 'en',setItem(){}};
global.BridgeI18n=require('../web/i18n.js');
global.BridgeIcons=require('../web/icons.js');
global.t_safe=value=>BridgeI18n.t(value);
global.TeamSwarmPanel=require('../web/team-swarm.js').TeamSwarmPanel;
const {TeamRunScreen,teamMemberAgentLabel}=require('../web/team.js');
global.teamMemberAgentLabel=teamMemberAgentLabel;
const {TeamSettingsDialog}=require('../web/team-chat.js');

const team={id:'team-1',name:'Team',version:4,leadId:'lead',policy:{swarmEnabled:false},members:[{id:'lead',sessionId:'session-1',name:'Lead',agentKind:'kimi',role:'lead',enabled:true}]};
function setupScreen(detail,request=async()=>({})){
  const root=new FakeNode('div'),screen=new TeamRunScreen({root,request});
  Object.assign(screen,{teamId:team.id,runId:detail.id,team,detail});screen.render();
  return {root,screen};
}
const detail=overrides=>({id:'run-1',teamId:team.id,goal:'保留完整的用户原话',status:'waiting_input',version:8,tasks:[],swarm:{schemaVersion:1},...overrides});

test('actual Run screen embeds Swarm only for opted-in runs and preserves draft text across events',()=>{
  const {root,screen}=setupScreen(detail());
  const panel=root.querySelector('.team-swarm-panel');assert.ok(panel);assert.match(text(panel),/Goal contract and evaluations/);
  const human=descendants(panel).find(node=>node.name==='human');human.value='A precise human criterion';human.listeners.input();
  screen.render();
  assert.equal(descendants(root).find(node=>node.name==='human').value,'A precise human criterion');
  screen.detail=detail({swarm:null});screen.render();assert.equal(root.querySelector('.team-swarm-panel'),null);
});

test('actual Run screen proposal request uses bound Run identity and schedules authoritative refresh',async()=>{
  const calls=[];
  const {root,screen}=setupScreen(detail(),async(path,options)=>{calls.push({path,...options});return {version:9,swarm:{schemaVersion:1,proposal:{id:'p1',contract:options.body.contract}}};});
  const refreshed=[];screen.scheduleRefetch=delay=>refreshed.push(delay);
  const propose=descendants(root).find(node=>node.tag==='button'&&node.textContent==='Propose acceptance contract');
  await propose.listeners.click();
  assert.equal(calls.length,1);assert.equal(calls[0].path,'/api/teams/team-1/runs/run-1/contract-proposals');
  assert.equal(calls[0].body.version,8);assert.equal(calls[0].body.contract.criteria[0].text,'保留完整的用户原话');
  assert.deepEqual(refreshed,[0]);
});

test('completed Run refetches late Swarm evaluation events after its terminal snapshot was synced',()=>{
  const {screen}=setupScreen(detail({status:'completed'}));
  screen.epoch='epoch';screen.lastSeq=2;screen.terminalSynced=true;
  const refreshed=[];screen.scheduleRefetch=delay=>refreshed.push(delay);
  screen.applyFrame({epoch:'epoch',sequence:3,rows:[{type:'swarm_updated',sequence:3,payload:{swarm:{schemaVersion:1,status:'pending_confirmation'}}}],meta:{status:'completed'}});
  assert.deepEqual(refreshed,[0]);
});

test('actual Team settings opt-in uses optimistic policy PATCH and disables duplicate toggles',async()=>{
  const calls=[],changes=[];let complete;
  const settings=new TeamSettingsDialog({request:async(path,options)=>{calls.push({path,...options});return new Promise(resolve=>{complete=resolve;});},onChange:value=>changes.push(value)});
  settings.team=team;settings.body=new FakeNode('div');settings.render();
  const enabled=descendants(settings.body).find(node=>node.type==='checkbox');assert.equal(enabled.checked,false);
  enabled.checked=true;const pending=enabled.onchange();
  assert.deepEqual(calls[0],{path:'/api/teams/team-1',method:'PATCH',body:{version:4,policy:{swarmEnabled:true}}});
  const disabled=descendants(settings.body).find(node=>node.type==='checkbox');assert.equal(disabled.disabled,true);
  disabled.checked=false;await disabled.onchange();assert.equal(calls.length,1);
  complete({...team,version:5,policy:{swarmEnabled:true}});await pending;
  assert.equal(changes.length,1);assert.equal(settings.team.version,5);
  assert.equal(descendants(settings.body).find(node=>node.type==='checkbox').checked,true);
});

test('actual Team settings keeps a failed opt-in visible without claiming it was enabled',async()=>{
  const settings=new TeamSettingsDialog({request:async()=>{throw new Error('Version conflict');}});
  settings.team=team;settings.body=new FakeNode('div');settings.render();
  const enabled=descendants(settings.body).find(node=>node.type==='checkbox');enabled.checked=true;await enabled.onchange();
  assert.match(text(settings.body),/Version conflict/);
  assert.equal(descendants(settings.body).find(node=>node.type==='checkbox').checked,false);
  assert.equal(descendants(settings.body).find(node=>node.type==='checkbox').disabled,false);
});
