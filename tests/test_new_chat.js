'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode{
  constructor(tag){
    this.tag=tag;this.children=[];this.dataset={};this.value='';this.textContent='';this.hidden=false;
    this.disabled=false;this.checked=false;this.open=false;this.classList={toggle(){}};
  }
  get options(){return this.children;}
  append(...nodes){
    this.children.push(...nodes);
    if(this.tag==='select'&&this.value===''&&nodes[0]?.value!==undefined)this.value=nodes[0].value;
  }
  replaceChildren(...nodes){this.children=[...nodes];this.value='';}
  querySelectorAll(){return[];}
  addEventListener(){}
  showModal(){this.open=true;}
  close(){this.open=false;}
}

function loadNewChat(data){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('let newChatProjects='),end=source.indexOf('const messageActions=',start);
  const nodes={},calls=[];
  for(const id of [
    'new-chat-error','new-chat-submit','new-chat-worktree-row','new-chat-worktree-base-row',
    'new-chat-project','new-chat-path','new-chat-dialog','new-chat-filter','new-chat-pin','new-chat-remove',
    'new-chat-title','new-chat-agent-row','new-chat-agent','new-chat-worktree',
    'new-chat-worktree-base','new-chat-custom-row','new-chat-custom-path','new-chat-desc',
    'new-chat-form','search','archived','new-chat','welcome-new-chat',
  ])nodes[id]=new FakeNode(id==='new-chat-project'||id==='new-chat-agent'?'select':'div');
  const closeButton=new FakeNode('button'),storage=new Map();
  let nextId=0;
  const context={
    $:id=>nodes[id],document:{querySelector:()=>closeButton},t:value=>value,
    Option:class{constructor(text,value){this.textContent=text;this.value=value;}},
    BridgeI18n:{agentTag:value=>value},listRows:[],currentId:null,currentHost:'local',window:{confirm:()=>true},
    localStorage:{getItem:key=>storage.get('local:'+key)||null,setItem:(key,value)=>storage.set('local:'+key,value)},
    sessionStorage:{getItem:key=>storage.get(key)||null,setItem:(key,value)=>storage.set(key,value),removeItem:key=>storage.delete(key)},
    api:async(path,body)=>{
      calls.push({path,body});
      if(path==='/api/projects')return data;
      return{id:'created-chat',host:'local',message:'created'};
    },
    teamRequest:async(path,options)=>{calls.push({path,options});return{status:'hidden'};},
    uuid:()=>`request-${++nextId}`,setLanguageBusy(){},toast(){},openChat:async()=>{},loadList:async()=>{},
  };
  vm.runInNewContext(source.slice(start,end)+'\nthis.openNewChat=openNewChat;this.customKey=newChatCustomKey;',context);
  return{nodes,calls,storage,open:()=>context.openNewChat(),customKey:context.customKey};
}

const savedData={
  projects:[{key:'codex:abc',cwd:'/work',name:'work',host:'local',agent:'codex'}],
  agents:[{kind:'codex',label:'Codex'}],
};

test('custom directory stays available without saved projects',async()=>{
  const {nodes,open,customKey}=loadNewChat({projects:[],agents:[{kind:'codex',label:'Codex'}]});
  await open();
  assert.equal(nodes['new-chat-project'].options.length,1);
  assert.equal(nodes['new-chat-project'].value,customKey);
  assert.equal(nodes['new-chat-custom-row'].hidden,false);
  assert.equal(nodes['new-chat-pin'].hidden,true);
  assert.equal(nodes['new-chat-submit'].disabled,false);
  assert.equal(nodes['new-chat-error'].textContent,'');
});

test('custom directory submits cwd and changes the idempotency key',async()=>{
  const {nodes,calls,storage,open,customKey}=loadNewChat(savedData);
  await open();
  nodes['new-chat-project'].value=customKey;nodes['new-chat-project'].onchange();
  nodes['new-chat-title'].value='自定义聊天';nodes['new-chat-custom-path'].value='relative/path';
  await nodes['new-chat-form'].onsubmit({preventDefault(){}});
  assert.equal(calls.filter(call=>call.path==='/api/sessions').length,0);
  assert.equal(nodes['new-chat-error'].textContent,'请输入电脑上已存在的目录绝对路径。');

  storage.set('new-chat-attempt',JSON.stringify({id:'old-request',project:'/tmp/new',cwd:'/tmp/old',title:'自定义聊天'}));
  nodes['new-chat-custom-path'].value=' /tmp/new ';
  await nodes['new-chat-form'].onsubmit({preventDefault(){}});
  const created=calls.find(call=>call.path==='/api/sessions');
  assert.equal(created.body.id,'request-1');
  assert.equal(created.body.project,'/tmp/new');
  assert.equal(created.body.cwd,'/tmp/new');
  assert.equal(created.body.title,'自定义聊天');
});

test('saved project creation keeps its existing request shape',async()=>{
  const {nodes,calls,open}=loadNewChat(savedData);
  await open();
  await nodes['new-chat-form'].onsubmit({preventDefault(){}});
  const created=calls.find(call=>call.path==='/api/sessions');
  assert.equal(created.body.project,'codex:abc');
  assert.equal(Object.hasOwn(created.body,'cwd'),false);
});

test('remove project calls the gateway and clears its pin',async()=>{
  const {nodes,calls,storage,open,customKey}=loadNewChat(savedData);
  storage.set('local:new-chat-pins',JSON.stringify([{key:'codex:abc',cwd:'/work',name:'work',host:'local',agent:'codex'}]));
  await open();
  assert.equal(nodes['new-chat-project'].options.length,2);
  await nodes['new-chat-remove'].onclick();
  const removed=calls.find(call=>call.path==='/api/projects/codex%3Aabc');
  assert.deepEqual({...removed.options},{method:'DELETE'});
  assert.equal(nodes['new-chat-project'].options.length,1);
  assert.equal(nodes['new-chat-project'].value,customKey);
  assert.deepEqual(JSON.parse(storage.get('local:new-chat-pins')),[]);
});

test('hidden projects are not restored from pins',async()=>{
  const {nodes,storage,open,customKey}=loadNewChat({projects:[],hiddenCwds:['/work'],agents:[{kind:'codex',label:'Codex'}]});
  storage.set('local:new-chat-pins',JSON.stringify([{key:'codex:abc',cwd:'/work',name:'work',host:'local',agent:'codex'}]));
  await open();
  assert.equal(nodes['new-chat-project'].options.length,1);
  assert.equal(nodes['new-chat-project'].value,customKey);
});
