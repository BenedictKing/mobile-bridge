'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');

// Run the waiting_input queue card with an isolated DOM and input API.
async function fixture(){
  const nodes=new Map(),listeners={},writes=[];
  const timelineOptions=[];
  const node=(tag='')=>({tagName:tag.toUpperCase(),children:[],value:'',checked:false,disabled:false,hidden:false,open:false,textContent:'',title:'',dataset:{},options:[],style:{},scrollTop:0,scrollHeight:0,clientHeight:0,parentElement:{},
    classList:{add(){},remove(){},toggle(){},contains:()=>true},addEventListener(){},setAttribute(){},replaceChildren(...values){this.children=values;},append(...values){this.children.push(...values);},querySelectorAll(){return [];},querySelector(){return {disabled:false};},
    showModal(){this.open=true;},close(){this.open=false;},dispatchEvent(){}});
  const html=fs.readFileSync(path.join(__dirname,'../web/index.html'),'utf8');
  for(const match of html.matchAll(/\bid="([^"]+)"/g))nodes.set(match[1],node());
  const storage=()=>({getItem(){return null;},setItem(){},removeItem(){}});
  const response=(data,status=200)=>({ok:status===200,status,json:async()=>data});
  const documentStub={hidden:false,documentElement:{dataset:{}},addEventListener:(name,fn)=>{(listeners[name]??=[]).push(fn);},
    getElementById:id=>nodes.get(id),querySelector:()=>null,querySelectorAll:()=>[],createElement:node};
  let inputHandler=null;
  class ChatTimeline{
    constructor(options){Object.assign(this,options);this.abort=new AbortController();timelineOptions.push(options);}
    async start(){}
    dispose(){this.abort.abort();}
    relabel(){}
  }
  const fetch=async(url,options={})=>{
    if(url.includes('/input?')){
      writes.push({url,body:JSON.parse(options.body)});
      if(inputHandler){
        const handled=inputHandler(JSON.parse(options.body));
        if(handled instanceof Error)return response({error:handled.message},400);
        return response(handled);
      }
      return response({status:'answered',id:'task-1',queued:true});
    }
    if(url.includes('/queue?')){writes.push({url,body:JSON.parse(options.body)});return response({status:'cancelled'});}
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
  await run(`openChat("11111111-1111-4111-8111-111111111111","local")`);
  await new Promise(setImmediate);
  const renderWaiting=async()=>{
    const view={id:'11111111-1111-4111-8111-111111111111',agent:'codex',host:'local',hostLabel:'此电脑',
      title:'Input test',cwd:'/work',connected:true,status:'idle',loadingHistory:false,activating:false,connecting:false,
      historyComplete:true,capabilities:['steer'],turns:[],requests:[],
      submissions:[{id:'task-1',text:'执行迁移',status:'waiting_input',editable:false,
        input:{id:'task-1',summary:'迁移前需要确认目标',version:1,expiresAt:1790000000,
          questions:[{id:'target',question:'迁移到哪个项目？',required:true,maxLength:20000},
                     {id:'note',question:'还有其他要求吗？',required:false,maxLength:500}]}}]};
    await run(`renderState(${JSON.stringify(view)})`);
    return nodes.get('queued').children[0];
  };
  return {nodes,writes,renderWaiting,setInputHandler:handler=>{inputHandler=handler;}};
}

const formOf=card=>card.children.find(n=>n.tagName==='FORM');
const fieldsOf=form=>[...form.children].filter(n=>n.tagName==='LABEL');
const flush=async()=>{await new Promise(setImmediate);await new Promise(setImmediate);};

test('waiting_input renders a question card instead of a plain queued row',async()=>{
  const ui=await fixture();
  const card=await ui.renderWaiting();
  assert.ok(card,'the card renders in the queue area');
  const deepText=node=>[String(node.textContent||''),...(node.children||[]).map(deepText)].join('');
  const text=deepText(card);
  assert.ok(text.includes('等待补充信息'),'the heading names the state');
  assert.ok(text.includes('迁移前需要确认目标'),'the task summary shows');
  assert.ok(text.includes('迁移到哪个项目？'),'required questions show');
  assert.ok(text.includes('（可选）'),'optional questions are marked');
  assert.ok(text.includes('过期时间'),'the expiry hint shows');
});

test('empty required answers are rejected locally without a request',async()=>{
  const ui=await fixture();
  const card=await ui.renderWaiting();
  formOf(card).onsubmit({preventDefault(){}});
  await new Promise(setImmediate);
  assert.equal(ui.writes.length,0,'nothing is posted');
  const error=formOf(card).children.find(n=>n.className==='error');
  assert.ok(String(error.textContent).includes('请回答所有问题'),'the validation error shows');
});

test('submitting answers posts once and shows the resume note',async()=>{
  const ui=await fixture();
  const card=await ui.renderWaiting();
  const form=formOf(card),fields=fieldsOf(form);
  fields[0].children[0].value='proj-x';
  fields[1].children[0].value='尽快';
  form.onsubmit({preventDefault(){}});
  await flush();
  assert.equal(ui.writes.length,1);
  assert.deepEqual(ui.writes[0].body,{id:'task-1',version:1,answers:{target:'proj-x',note:'尽快'}});
  assert.ok(card.children.some(n=>n.className==='muted'&&String(n.textContent).includes('已提交，等待恢复')),
    'the resume note appears immediately');
});

test('a duplicate answer and a server error are both surfaced',async()=>{
  const ui=await fixture();
  ui.setInputHandler(()=>({status:'duplicate',id:'task-1'}));
  let card=await ui.renderWaiting();
  let form=formOf(card);
  fieldsOf(form)[0].children[0].value='proj-x';
  form.onsubmit({preventDefault(){}});
  await flush();
  assert.match(ui.nodes.get('toast').textContent,/回答已提交过/);
  assert.ok(card.children.some(n=>String(n.textContent).includes('已提交，等待恢复')));

  ui.setInputHandler(body=>Error(body.answers.target==='again'?'问题已变化，请刷新后重新回答':'其他错误'));
  card=await ui.renderWaiting();
  form=formOf(card);
  fieldsOf(form)[0].children[0].value='again';
  form.onsubmit({preventDefault(){}});
  await flush();
  const error=form.children.find(n=>n.className==='error');
  assert.ok(String(error.textContent).includes('问题已变化'),'the server error shows on the card');
  const submit=form.children.flatMap(n=>n.children||[]).find(n=>String(n.className).includes('plain')&&n.type==='submit');
  assert.ok(submit.disabled===false,'the submit button recovers after an error');
});

test('cancelling a waiting task goes through the queue delete action',async()=>{
  const ui=await fixture();
  const card=await ui.renderWaiting();
  const form=formOf(card);
  const remove=form.children.flatMap(n=>n.children||[]).find(n=>n.type==='button'&&String(n.textContent).includes('取消任务'));
  assert.ok(remove,'the cancel action renders');
  remove.onclick();
  await flush();
  assert.equal(ui.writes.length,1);
  assert.deepEqual(ui.writes[0].body,{id:'task-1',action:'delete'});
  assert.equal(ui.nodes.get('queued').children.length,0,'the card disappears after cancelling');
});
