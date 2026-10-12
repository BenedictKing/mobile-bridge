'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');

// 定时发现的前端侧：activity 轮询携带的 listRevision 变化时自动重载会话列表，
// 空列表也继续轮询，让网页之外新开的第一个会话能被发现。
async function fixture(){
  const nodes=new Map();
  const node=(tag='')=>({tagName:tag.toUpperCase(),children:[],value:'',checked:false,disabled:false,hidden:false,open:false,textContent:'',title:'',dataset:{},options:[],style:{},scrollTop:0,parentElement:{},
    classList:{add(){},remove(){},toggle(){}},addEventListener(){},setAttribute(){},replaceChildren(...values){this.children=values;},append(...values){this.children.push(...values);},querySelectorAll(){return [];},querySelector(){return {disabled:false,prepend(){}};},
    showModal(){this.open=true;},close(){this.open=false;}});
  const html=fs.readFileSync(path.join(__dirname,'../web/index.html'),'utf8');
  for(const match of html.matchAll(/\bid="([^"]+)"/g))nodes.set(match[1],node());
  const storage=()=>({getItem(){return null;},setItem(){},removeItem(){}});
  const state={revision:null,activityCalls:0,listCalls:0,listRows:[]};
  const response=(data,status=200)=>({ok:status===200,status,json:async()=>data});
  const context=vm.createContext({document:{addEventListener(){},documentElement:{},hidden:false,getElementById:id=>nodes.get(id),querySelector:()=>({disabled:false}),querySelectorAll:()=>[],createElement:node},
    renderMarkdown(){},window:{addEventListener(){}},localStorage:storage(),sessionStorage:storage(),
    location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},setTimeout(){},clearTimeout(){},setInterval(){},
    ChatTimeline:class{constructor(){this.abort=new AbortController();}async start(){}dispose(){this.abort.abort();}relabel(){}},
    fetch:async url=>{
      if(url==='/api/auth')return response({authenticated:false});
      if(url==='/api/activity'){state.activityCalls++;return response({sessions:[],listRevision:state.revision});}
      if(url==='/api/sessions?q=&offset=0&archived=false'){state.listCalls++;return response({sessions:state.listRows,unavailableHosts:[]});}
      return response({});
    }});
  for(const file of ['web/i18n.js','web/account.js','web/modes.js','web/attachments.js','web/activity.js','web/fast-mode.js','web/message-actions.js','web/app.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,'..',file),'utf8'),context);
  await new Promise(setImmediate);
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};
  return {nodes,
    run:code=>vm.runInContext(code,context),
    settle,
    setRevision:value=>{state.revision=value;},
    setListRows:rows=>{state.listRows=rows;},
    counts:()=>({activityCalls:state.activityCalls,listCalls:state.listCalls})};
}

function sessionButtons(root){
  const found=[];
  for(const child of root.children||[]){
    if(child.dataset&&child.dataset.id)found.push(child);
    found.push(...sessionButtons(child));
  }
  return found;
}

test('a listRevision change reloads the list once and renders the discovered chat',async()=>{
  const ui=await fixture();
  ui.nodes.get('app').hidden=false;
  ui.run("listRows=[{id:'known',host:'local',title:'旧会话',recency:1,projectKey:'local|p',projectName:'p'}]");
  // 首次见到的版本只记录基线，相同版本也不重载。
  ui.setRevision(3);
  await ui.run('refreshActivity()');await ui.settle();
  ui.setRevision(3);
  await ui.run('refreshActivity()');await ui.settle();
  assert.equal(ui.counts().listCalls,0);
  // 版本变化：Agent 侧新开会话，列表自动重载并渲染新行。
  ui.setRevision(4);
  ui.setListRows([{id:'new',host:'local',title:'终端新会话',recency:Date.now(),projectKey:'local|t',projectName:'t'}]);
  await ui.run('refreshActivity()');await ui.settle();
  assert.equal(ui.counts().listCalls,1);
  const rendered=sessionButtons(ui.nodes.get('sessions')).map(button=>button.dataset.id);
  assert.deepEqual(rendered,['new']);
  // 版本不再变化时不重复重载。
  ui.setRevision(4);
  await ui.run('refreshActivity()');await ui.settle();
  assert.equal(ui.counts().listCalls,1);
});

test('empty list keeps polling activity so the first external session is found',async()=>{
  const ui=await fixture();
  ui.nodes.get('app').hidden=false;
  ui.run('listRows=[]');
  ui.setRevision(1);
  await ui.run('refreshActivity()');await ui.settle();
  assert.equal(ui.counts().activityCalls,1);
  assert.equal(ui.counts().listCalls,0);
  ui.setRevision(2);
  ui.setListRows([{id:'first',host:'local',title:'第一个会话',recency:Date.now(),projectKey:'local|t',projectName:'t'}]);
  await ui.run('refreshActivity()');await ui.settle();
  assert.equal(ui.counts().listCalls,1);
  assert.deepEqual(sessionButtons(ui.nodes.get('sessions')).map(button=>button.dataset.id),['first']);
});

test('servers without discovery keep working: no revision field means no reload',async()=>{
  const ui=await fixture();
  ui.nodes.get('app').hidden=false;
  ui.run("listRows=[{id:'known',host:'local',title:'旧会话',recency:1,projectKey:'local|p',projectName:'p'}]");
  ui.setRevision(undefined);  // 旧网关响应里没有 listRevision 字段
  await ui.run('refreshActivity()');await ui.settle();
  await ui.run('refreshActivity()');await ui.settle();
  assert.equal(ui.counts().listCalls,0);
});
