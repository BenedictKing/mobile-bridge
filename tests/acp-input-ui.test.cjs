'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');

// Exercise the ACP elicitation form card and failed queue rows with an
// isolated DOM, reusing the task-input UI fixture shape.
async function fixture(){
  const nodes=new Map(),listeners={},writes=[];
  const node=(tag='')=>({tagName:tag.toUpperCase(),children:[],value:'',checked:false,disabled:false,hidden:false,open:false,textContent:'',title:'',dataset:{},options:[],style:{},scrollTop:0,scrollHeight:0,clientHeight:0,parentElement:{},
    classList:{add(){},remove(){},toggle(){},contains:()=>true},addEventListener(){},setAttribute(){},replaceChildren(...values){this.children=values;},append(...values){this.children.push(...values);},prepend(...values){this.children.unshift(...values);},querySelectorAll(){return [];},querySelector(){return {disabled:false};},
    showModal(){this.open=true;},close(){this.open=false;},dispatchEvent(){}});
  const html=fs.readFileSync(path.join(__dirname,'../web/index.html'),'utf8');
  for(const match of html.matchAll(/\bid="([^"]+)"/g))nodes.set(match[1],node());
  const storage=()=>({getItem(){return null;},setItem(){},removeItem(){}});
  const response=(data,status=200)=>({ok:status===200,status,json:async()=>data});
  const documentStub={hidden:false,documentElement:{dataset:{}},addEventListener:(name,fn)=>{(listeners[name]??=[]).push(fn);},
    getElementById:id=>nodes.get(id),querySelector:()=>null,querySelectorAll:()=>[],createElement:node};
  let respondHandler=null;
  class ChatTimeline{
    constructor(options){Object.assign(this,options);this.abort=new AbortController();}
    async start(){}
    dispose(){this.abort.abort();}
    relabel(){}
  }
  const fetch=async(url,options={})=>{
    if(url.includes('/queue')){
      writes.push({url,body:JSON.parse(options.body)});
      return response({status:'accepted',id:JSON.parse(options.body).id});
    }
    if(url.includes('/respond')){
      writes.push({url,body:JSON.parse(options.body)});
      if(respondHandler){
        const handled=respondHandler(JSON.parse(options.body));
        if(handled instanceof Error)return response({error:handled.message},400);
        return response(handled);
      }
      return response({ok:true});
    }
    if(url==='/api/auth')return response({authenticated:false});
    if(url.includes('/api/activity'))return response({sessions:[],listRevision:null});
    if(url.includes('/reconnect'))return response({ok:true});
    if(url.includes('/notifications?'))return response({available:true,watching:false,notifyOnCompletion:false,requests:'inherit',completion:'inherit'});
    if(url.includes('/permissions?'))return response({supported:false,options:[],current:null});
    if(url.includes('/api/sessions?q='))return response({sessions:[],unavailableHosts:[]});
    return response({});
  };
  const context=vm.createContext({document:documentStub,renderMarkdown(){},window:{addEventListener(){}},
    localStorage:storage(),sessionStorage:storage(),navigator:{language:'zh-CN'},
    location:{hash:'',pathname:'/',search:''},history:{replaceState(){}},
    setTimeout(){return 0;},clearTimeout(){},setInterval(){return 0;},
    crypto:{getRandomValues:()=>new Uint8Array(16)},
    Option:class{constructor(label,value){this.label=label;this.value=value;}},
    BridgeTransport:{wake(){},subscribe:()=>()=>{}},ChatTimeline,fetch,EventSource:undefined,AbortController,URLSearchParams});
  for(const file of ['web/i18n.js','web/account.js','web/modes.js','web/attachments.js','web/activity.js','web/fast-mode.js','web/message-actions.js','web/app.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,'..',file),'utf8'),context);
  await new Promise(setImmediate);
  const run=code=>vm.runInContext(code,context);
  await run(`openChat("22222222-2222-4222-8222-222222222222","local")`);
  await new Promise(setImmediate);
  const renderView=async view=>{
    await run(`renderState(${JSON.stringify(view)})`);
    return view;
  };
  return {nodes,writes,renderView,setRespondHandler:handler=>{respondHandler=handler;}};
}

const baseView=extra=>({id:'22222222-2222-4222-8222-222222222222',agent:'zcode',host:'local',hostLabel:'此电脑',
  title:'Form test',cwd:'/work',connected:true,status:'active',loadingHistory:false,activating:false,connecting:false,
  historyComplete:true,capabilities:[],turns:[],requests:[],submissions:[],...extra});
const deepText=node=>[String(node.textContent||''),...(node.children||[]).map(deepText)].join('');
const flush=async()=>{await new Promise(setImmediate);await new Promise(setImmediate);};
const byTag=(node,tag)=>[...(node.children||[])].filter(n=>n.tagName===tag.toUpperCase());

const FORM_REQUEST={id:'r1',method:'acp/requestInput',supported:true,params:{
  message:'需要确认部署方式。',
  schema:{type:'object',required:['q_0'],properties:{
    q_0:{type:'string',title:'部署环境',oneOf:[{const:'stage',title:'预发'},{const:'prod',title:'生产'}]},
    q_0_other:{type:'string',title:'Other'},
    q_1:{type:'array',title:'通知渠道',items:{anyOf:[{const:'bark',title:'Bark'},{const:'ntfy',title:'ntfy'}]}},
  }}}};

test('ACP form request renders selects, free text and multi-select',async()=>{
  const ui=await fixture();
  await ui.renderView(baseView({requests:[FORM_REQUEST]}));
  const card=ui.nodes.get('approvals').children[0];
  assert.ok(card,'the form card renders');
  const text=deepText(card);
  assert.ok(text.includes('Agent 需要你的回复'),'the heading names the ask');
  assert.ok(text.includes('需要确认部署方式。'),'the message shows');
  assert.ok(text.includes('部署环境'),'the question title shows');
  assert.ok(text.includes('通知渠道'),'the multi-select title shows');
  const select=deepCollect(card,'SELECT')[0];
  assert.ok(select,'the oneOf question renders a dropdown');
  assert.equal(select.children.length,3,'placeholder plus both options');
});

test('required answer must be picked before submitting',async()=>{
  const ui=await fixture();
  await ui.renderView(baseView({requests:[FORM_REQUEST]}));
  const card=ui.nodes.get('approvals').children[0];
  const form=card.children.find(n=>n.tagName==='FORM');
  form.onsubmit({preventDefault(){}});
  await flush();
  assert.equal(ui.writes.length,0,'nothing is posted');
  const error=form.children.find(n=>n.className==='error');
  assert.ok(String(error.textContent).includes('请填写所有必填项'),'the validation error shows');
});

test('accept posts the structured elicitation answer',async()=>{
  const ui=await fixture();
  await ui.renderView(baseView({requests:[FORM_REQUEST]}));
  const card=ui.nodes.get('approvals').children[0];
  const select=deepCollect(card,'SELECT')[0];
  select.value='prod';
  const checkboxes=deepCollect(card,'INPUT').filter(n=>n.type==='checkbox');
  assert.equal(checkboxes.length,2,'both multi-select options render');
  checkboxes[0].checked=true;  // bark
  const form=card.children.find(n=>n.tagName==='FORM');
  form.onsubmit({preventDefault(){}});
  await flush();
  assert.equal(ui.writes.length,1);
  assert.deepEqual(ui.writes[0].body,{requestId:'r1',response:{action:'accept',content:{q_0:'prod',q_1:['bark']}}});
  assert.ok(card.children.some(n=>n.className==='muted'&&String(n.textContent).includes('已发送，等待桌面确认')),
    'the confirmation note appears');
});

test('decline posts the decline action',async()=>{
  const ui=await fixture();
  await ui.renderView(baseView({requests:[FORM_REQUEST]}));
  const card=ui.nodes.get('approvals').children[0];
  const decline=card.children.find(n=>n.tagName==='FORM').children
    .find(n=>n.tagName==='DIV').children.find(n=>String(n.textContent)==='拒绝');
  decline.onclick();
  await flush();
  assert.deepEqual(ui.writes[0].body,{requestId:'r1',response:{action:'decline'}});
});

test('failed queue row shows the error and retry action',async()=>{
  const ui=await fixture();
  await ui.renderView(baseView({status:'idle',submissions:[
    {id:'q1',text:'继续部署',status:'failed',editable:true,error:'A prompt is already running'}]}));
  const card=ui.nodes.get('queued').children[0];
  const text=deepText(card);
  assert.ok(text.includes('发送失败'),'the failure heading shows');
  assert.ok(text.includes('A prompt is already running'),'the agent error shows');
  assert.ok(text.includes('重试发送'),'retry is offered');
  assert.ok(text.includes('修改')&&text.includes('删除'),'edit and delete remain available');
  const retry=card.children.find(n=>n.className==='queue-actions').children[0];
  await retry.onclick();
  await flush();
  assert.deepEqual(ui.writes[0].body,{id:'q1',action:'send'},'retry uses the queue send action');
});

const deepCollect=(node,tag)=>[...(node.children||[])].flatMap(n=>[n,...deepCollect(n,tag)])
  .filter(n=>n.tagName===tag.toUpperCase());
