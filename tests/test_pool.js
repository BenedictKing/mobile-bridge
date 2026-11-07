'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {PoolPanel}=require('../web/pool.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.className='';this.textContent='';}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  setAttribute(){}
}
function find(node,predicate,rows=[]){
  if(predicate(node))rows.push(node);
  for(const child of node.children||[])find(child,predicate,rows);
  return rows;
}
function fixture(kind){
  const root=new FakeNode('div'),calls=[];
  const panel=new PoolPanel({
    root,kind,label:kind,
    read:async()=>({}),
    request:async(action,payload)=>{
      calls.push({action,payload});
      return {...panel.value,thresholds:payload.thresholds||panel.value.thresholds};
    },
  });
  panel.value={kind,autoSwitch:true,accounts:[],history:[],thresholds:{switch:99,longTerm:99,resetCard:98}};
  panel.render();
  return {panel,root,calls};
}
function inputs(root){return find(root,node=>node.className==='pool-threshold-input');}

test('threshold fields are per agent and reset cards are zcode-only',()=>{
  globalThis.BridgeI18n={t:value=>value};
  globalThis.document={createElement:tag=>new FakeNode(tag)};
  const zcode=fixture('zcode'),kimi=fixture('kimi');
  assert.deepEqual(inputs(zcode.root).map(input=>input.dataset.key),['switch','longTerm','resetCard']);
  assert.deepEqual(inputs(kimi.root).map(input=>input.dataset.key),['switch','longTerm']);
  assert.equal(inputs(zcode.root)[2].value,98);
});

test('saving thresholds sends percentages through the settings action',async()=>{
  const {panel,root,calls}=fixture('zcode');
  const controls=inputs(root);
  controls[0].value='97.5';controls[1].value='99.5';controls[2].value='96';
  const save=find(root,node=>node.textContent==='保存阈值')[0];
  await save.onclick();
  assert.equal(calls.length,1);
  assert.equal(calls[0].action,'settings');
  assert.deepEqual(calls[0].payload,{thresholds:{switch:97.5,longTerm:99.5,resetCard:96}});
  assert.equal(panel.error,'');
});

test('invalid percentages stay local and show an error',async()=>{
  const {panel,root,calls}=fixture('kimi');
  inputs(root)[0].value='0';
  const save=find(root,node=>node.textContent==='保存阈值')[0];
  await save.onclick();
  assert.equal(calls.length,0);
  assert.equal(panel.error,'阈值必须大于 0 且不超过 100');
});

test('login copy button hands the challenge URL to BridgeClipboard',async()=>{
  globalThis.BridgeI18n={t:value=>value};
  globalThis.document={createElement:tag=>new FakeNode(tag)};
  const {panel,root}=fixture('kimi');
  const copies=[];
  globalThis.window={BridgeClipboard:{copy:async(getText,button)=>{copies.push({text:await getText(),button});}}};
  const url='https://example.com/device?user_code=ABCD';
  panel.login={phase:'waiting',challenge:{url,userCode:'ABCD'},deadline:Date.now()+60000,provider:'',error:''};
  panel.render();
  const copy=find(root,node=>node.textContent==='复制链接')[0];
  assert.ok(copy,'login box should render a copy button');
  await copy.onclick();
  assert.equal(copies.length,1);
  assert.equal(copies[0].text,url);
  assert.equal(copies[0].button,copy);
});

test('a failed login cancellation keeps the box open and shows the error',async()=>{
  globalThis.BridgeI18n={t:value=>value};
  globalThis.document={createElement:tag=>new FakeNode(tag)};
  const {panel,root}=fixture('kimi');
  panel.request=async action=>{if(action==='login/cancel')throw Error('网关不可达');return {};};
  panel.login={phase:'waiting',challenge:{url:'https://example.com/device'},deadline:Date.now()+60000,provider:'',error:''};
  panel.render();
  await find(root,node=>node.textContent==='取消登录')[0].onclick();
  assert.ok(panel.login,'登录框必须保留：服务端可能仍在等待授权');
  assert.equal(panel.error,'网关不可达');
  assert.equal(find(root,node=>node.textContent==='取消登录').length,1,'按钮必须恢复可点');
});

test('a successful login cancellation closes the box and confirms',async()=>{
  globalThis.BridgeI18n={t:value=>value};
  globalThis.document={createElement:tag=>new FakeNode(tag)};
  const {panel,root}=fixture('kimi');
  const calls=[];
  panel.request=async action=>{calls.push(action);return {};};
  panel.login={phase:'waiting',challenge:{url:'https://example.com/device'},deadline:Date.now()+60000,provider:'',error:''};
  panel.render();
  await find(root,node=>node.textContent==='取消登录')[0].onclick();
  assert.equal(panel.login,null);
  assert.deepEqual(calls,['login/cancel']);
  assert.equal(find(root,node=>node.textContent==='登录已取消').length,1);
});

test('successful pool actions confirm, failures never report success',async()=>{
  globalThis.BridgeI18n={t:value=>value};
  globalThis.document={createElement:tag=>new FakeNode(tag)};
  const notices=[];
  globalThis.BridgeFeedback={ok:text=>notices.push(text)};
  try{
    const {panel}=fixture('kimi');
    await panel.perform('refresh');
    await panel.perform('switch',{id:'a',confirmed:true});
    await panel.perform('settings',{autoSwitch:false});
    assert.deepEqual(notices,['额度已刷新','账号已切换','自动切换设置已保存']);
    // 阈值保存已有 notice，不应再叠一条通用提示。
    await panel.perform('settings',{thresholds:{switch:99}});
    assert.equal(notices.length,3);
    panel.request=async()=>{throw Error('网关不可达');};
    await panel.perform('remove',{id:'a',confirmed:true});
    assert.equal(notices.length,3,'失败时不得报成功');
    assert.equal(panel.error,'网关不可达');
  }finally{delete globalThis.BridgeFeedback;}
});

test('shared success feedback delegates to the main toast, with a #toast fallback',()=>{
  const vm=require('node:vm'),fs=require('node:fs'),path=require('node:path');
  const toasts=[],timers=[],classes=[],button={classList:{add:name=>classes.push('+'+name),remove:name=>classes.push('-'+name)}};
  const context={
    window:{toast:text=>toasts.push(text)},
    document:{getElementById:()=>null},
    BridgeI18n:{t:source=>source==='已保存'?'Saved':source},
    setTimeout:(fn,ms)=>{timers.push({fn,ms});return timers.length;},
    clearTimeout:()=>{},
  };
  vm.createContext(context);
  vm.runInContext(fs.readFileSync(path.join(__dirname,'..','web','message-actions.js'),'utf8'),context);
  context.window.BridgeFeedback.ok('已保存',button);
  assert.deepEqual(toasts,['Saved'],'成功提示应复用主界面 toast');
  assert.deepEqual(classes,['+copied']);
  timers[0].fn();
  assert.deepEqual(classes,['+copied','-copied']);
  // 没有 app.js 的全局 toast 时，退回页面自带的 #toast 元素。
  const fallback={hidden:true,textContent:''};
  context.window.toast=undefined;
  context.document.getElementById=id=>id==='toast'?fallback:null;
  context.window.BridgeFeedback.ok('额度已刷新');
  assert.equal(fallback.hidden,false);
  assert.equal(fallback.textContent,'额度已刷新');
});
