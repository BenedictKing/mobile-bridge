'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.className='';this.textContent='';}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  setAttribute(){}
}
function find(node,cls){
  const rows=[];
  const visit=current=>{if(typeof current==='string')return;if(current.className.split(/\s+/).includes(cls))rows.push(current);for(const child of current.children)visit(child);};
  visit(node);return rows;
}
function loadList(listMode,overrides={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const elStart=source.indexOf('function el('),elEnd=source.indexOf('\n',elStart);
  const start=source.indexOf('const TIME_BUCKETS='),end=source.indexOf('async function loadList',start);
  const sessions=new FakeNode('div');
  const context={
    listMode,listRows:[{id:'chat-1',host:'local',recency:Date.now(),title:'最近聊天',projectKey:'codex|/work',projectName:'work',agent:'codex'}],
    projectDirectory:[{key:'codex|/work',cwd:'/work',name:'work',agent:'codex'}],
    listQuery:'',currentId:null,currentHost:'local',currentTeam:null,teamChat:null,
    $:id=>{assert.equal(id,'sessions');return sessions;},
    document:{createElement:tag=>new FakeNode(tag)},localStorage:{getItem:()=>null,setItem(){}},
    t:value=>value,BridgeI18n:{agentLabel:value=>value,agentTag:value=>value},
    dateText:()=> '刚刚',renderActivity(){},openNewChat(){},openChat:async()=>{},
  };
  Object.assign(context,overrides);
  vm.runInNewContext(source.slice(elStart,elEnd)+'\n'+source.slice(start,end)+'\nthis.render=renderList;',context);
  context.render();
  return sessions;
}

test('recent activity does not pin default project teams above time buckets',()=>{
  const sessions=loadList('recent');
  assert.equal(find(sessions,'default-team').length,0);
  assert.equal(find(sessions,'project-group').length,1);
  assert.equal(find(sessions,'session').length,1);
});

test('project groups retain their default team entry',()=>{
  const sessions=loadList('project');
  assert.equal(find(sessions,'default-team').length,1);
  assert.equal(find(sessions,'session').length,2);
});

test('project groups hide only the pinned team lead from conversation history',()=>{
  const team={id:'team-1',name:'work团队'};
  const listRows=[
    {id:'lead-chat',host:'local',recency:Date.now(),title:'团队 Lead 会话',projectKey:'codex|/work',projectName:'work',agent:'kimi'},
    {id:'member-chat',host:'local',recency:Date.now(),title:'成员会话',projectKey:'codex|/work',projectName:'work',agent:'mcode'},
  ];
  const teamChat={teamForProject:()=>team,teamForSession:id=>id==='lead-chat'?team:null};
  const grouped=loadList('project',{listRows,teamChat});
  assert.equal(find(grouped,'default-team').length,1);
  const rows=find(grouped,'session');
  assert.equal(rows.length,2);
  assert.equal(rows[1].children[0].textContent,'成员会话');
  const recent=loadList('recent',{listRows,teamChat});
  assert.equal(find(recent,'session').length,2);
});

// 同一目录跨 Agent：Codex 桌面项目（groupKey=项目 ID）与该目录下其他
// Agent 的会话（projectKey=目录）合并为一个组、一个组头、一个团队入口。
test('agents in one project directory share a single group',()=>{
  const listRows=[
    {id:'codex-chat',host:'local',recency:Date.now(),title:'Codex 会话',projectKey:'local|p1',projectName:'mobile-bridge',groupKey:'local|p1',groupName:'mobile-bridge',cwd:'/w/mobile-bridge',agent:'codex'},
    {id:'kimi-chat',host:'local',recency:Date.now()-1000,title:'Kimi 会话',projectKey:'local|/w/mobile-bridge',projectName:'mobile-bridge',groupKey:'local|p1',groupName:'mobile-bridge',cwd:'/w/mobile-bridge',agent:'kimi'},
  ];
  const projectDirectory=[{key:'acp:h1',cwd:'/w/mobile-bridge',name:'mobile-bridge',agent:'kimi'}];
  const sessions=loadList('project',{listRows,projectDirectory});
  assert.equal(find(sessions,'project-group').length,1);
  // default-team 按钮也带 session class：2 条会话 + 1 个团队入口。
  assert.equal(find(sessions,'session').length,3);
  assert.equal(find(sessions,'default-team').length,1);
  assert.equal(find(sessions,'group-name')[0].textContent,'mobile-bridge · 此电脑');
});

// 同名不同目录不合组；不同主机的同路径也各自成组（groupKey 缺省回退
// projectKey，其中已含主机前缀）。
test('same project name or path on different directories and hosts stays separate',()=>{
  const listRows=[
    {id:'a',host:'local',recency:Date.now(),title:'A',projectKey:'local|/a/work',projectName:'work',cwd:'/a/work',agent:'kimi'},
    {id:'b',host:'local',recency:Date.now(),title:'B',projectKey:'local|/b/work',projectName:'work',cwd:'/b/work',agent:'kimi'},
    {id:'c',host:'nas',recency:Date.now(),title:'C',projectKey:'nas|/work',projectName:'work',hostLabel:'NAS',cwd:'/work',agent:'kimi'},
  ];
  const sessions=loadList('project',{listRows,projectDirectory:[]});
  assert.equal(find(sessions,'project-group').length,3);
});

test('merged group keys collapse state and add button by groupKey',()=>{
  const listRows=[
    {id:'codex-chat',host:'local',recency:Date.now(),title:'Codex 会话',projectKey:'local|p1',projectName:'P',groupKey:'local|p1',groupName:'P',cwd:'/p',agent:'codex'},
    {id:'kimi-chat',host:'local',recency:Date.now()-1000,title:'Kimi 会话',projectKey:'local|/p',projectName:'P',groupKey:'local|p1',groupName:'P',cwd:'/p',agent:'kimi'},
  ];
  const stored=new Map();
  let opened=null;
  const sessions=loadList('project',{listRows,projectDirectory:[],
    localStorage:{getItem:key=>stored.get(key)??null,setItem:(key,value)=>stored.set(key,value)},
    openNewChat:chat=>{opened=chat;}});
  const group=find(sessions,'project-group')[0];
  group.ontoggle();
  assert.deepEqual([...stored.keys()],['group:local|p1']);
  find(sessions,'group-add')[0].onclick({preventDefault(){},stopPropagation(){}});
  assert.equal(opened&&opened.id,'codex-chat');
});

test('merged group shares one show-more paging budget',()=>{
  const listRows=Array.from({length:7},(_,index)=>({
    id:'chat-'+index,host:'local',recency:Date.now()-index*1000,title:'会话'+index,
    projectKey:index%2?'local|/p':'local|p1',projectName:'P',groupKey:'local|p1',groupName:'P',cwd:'/p',agent:index%2?'kimi':'codex',
  }));
  const sessions=loadList('project',{listRows,projectDirectory:[]});
  assert.equal(find(sessions,'project-group').length,1);
  assert.equal(find(sessions,'session').length,5);
  const more=find(sessions,'group-more')[0];
  assert.equal(more.textContent,'显示更多 · 2');
  more.onclick();
  assert.equal(find(sessions,'session').length,7);
  const less=find(sessions,'group-more')[0];
  assert.equal(less.textContent,'收起');
  less.onclick();
  assert.equal(find(sessions,'session').length,5);
  assert.equal(find(sessions,'group-more')[0].textContent,'显示更多 · 2');
});

test('show more and show less share one row at opposite ends',()=>{
  const listRows=Array.from({length:16},(_,index)=>({
    id:'chat-'+index,host:'local',recency:Date.now()-index*1000,title:'会话'+index,
    projectKey:'local|/p',projectName:'P',groupKey:'local|p1',groupName:'P',cwd:'/p',agent:'kimi',
  }));
  const sessions=loadList('project',{listRows,projectDirectory:[]});
  find(sessions,'group-more')[0].onclick();
  const controls=find(sessions,'group-pagination');
  assert.equal(controls.length,1);
  assert.deepEqual(Array.from(controls[0].children,button=>button.textContent),['显示更多 · 1','收起']);
  const css=readFileSync(require.resolve('../web/style.css'),'utf8');
  assert.match(css,/\.group-pagination\{display:flex;align-items:center;justify-content:space-between\}/);
});

test('working status and scroll hints float above the timeline instead of consuming flow',()=>{
  const html=readFileSync(require.resolve('../web/index.html'),'utf8');
  const shell=html.match(/<div class="timeline-shell">[\s\S]*?<details id="user-jump"/)[0];
  assert(shell.indexOf('id="timeline"')<shell.indexOf('class="timeline-overlay"'));
  assert(shell.indexOf('class="timeline-overlay"')<shell.indexOf('id="working"'));
  assert(shell.indexOf('id="working"')<shell.indexOf('id="newer"'));
  const css=readFileSync(require.resolve('../web/presentation.css'),'utf8');
  assert.match(css,/\.timeline-shell\{position:relative;flex:1;min-height:0\}/);
  assert.match(css,/\.timeline-overlay\{position:absolute;z-index:3;[^}]*display:flex;[^}]*justify-content:space-between/);
  assert.match(css,/\.timeline-overlay\{[^}]*padding:0 max\(24px,calc\(\(100% - 780px\)\/2\)\) 8px/);
  assert.match(css,/\.timeline-overlay\{padding-left:[^}]*padding-bottom:8px\}/);
  assert.match(css,/\.timeline-overlay \.newer\{[^}]*pointer-events:auto/);
});
