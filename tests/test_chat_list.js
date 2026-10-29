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
function loadList(listMode){
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
