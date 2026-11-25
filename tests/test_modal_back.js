'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const path=require('node:path');

// 假历史栈：back() 像浏览器一样异步派发 popstate，监听器支持 stopImmediatePropagation。
const windowListeners=[],documentListeners=[];
const history={entries:[{state:null,url:'/'}],index:0,
  get state(){return this.entries[this.index].state;},
  pushState(state,_,url){this.entries.splice(this.index+1,Infinity,{state:structuredClone(state),url});this.index++;},
  replaceState(state,_,url){this.entries[this.index]={state:structuredClone(state),url};},
  back(){setImmediate(()=>{if(this.index>0){this.index--;firePop();}});},
};
function firePop(){
  let stopped=false;
  const event={stopImmediatePropagation(){stopped=true;}};
  for(const handler of windowListeners){handler(event);if(stopped)break;}
}
class HTMLDialogElement {
  constructor(){this.open=false;this.isConnected=true;this.listeners={};}
  addEventListener(kind,handler){(this.listeners[kind]??=[]).push(handler);}
  showModal(){this.open=true;}
  close(){if(!this.open)return;this.open=false;setImmediate(()=>this.dispatchEvent({type:'close'}));}
  dispatchEvent(event){
    event.target=this;
    if(event.type==='close')for(const handler of documentListeners)handler(event);
    for(const handler of this.listeners[event.type]||[])handler(event);
    return !event.defaultPrevented;
  }
}
global.HTMLDialogElement=HTMLDialogElement;
global.Event=class{constructor(type,{cancelable}={}){this.type=type;this.cancelable=cancelable;this.defaultPrevented=false;}preventDefault(){if(this.cancelable)this.defaultPrevented=true;}};
global.history=history;
global.location={get href(){return history.entries[history.index].url;}};
Object.defineProperty(globalThis,'navigator',{value:{userActivation:{isActive:true}},configurable:true});
global.window={addEventListener:(kind,handler)=>{if(kind==='popstate')windowListeners.push(handler);}};
global.document={addEventListener:(kind,handler,capture)=>{if(kind==='close'&&capture)documentListeners.push(handler);}};
require(path.join(__dirname,'..','web','modal-back.js'));
// app.js 的路由监听在 modal-back 之后注册：记录哪些 popstate 漏到了路由。
const routed=[];
window.addEventListener('popstate',()=>routed.push(history.entries[history.index].url));
const settle=()=>new Promise(resolve=>setTimeout(resolve,5));
function reset(url='/'){history.entries=[{state:null,url}];history.index=0;routed.length=0;navigator.userActivation.isActive=true;}

test('back gesture closes the open dialog instead of routing',async()=>{
  reset('/#chat');
  const dialog=new HTMLDialogElement();dialog.showModal();
  assert.equal(history.entries.length,2);
  history.back();await settle();
  assert.equal(dialog.open,false);
  assert.deepEqual(routed,[]);
  assert.equal(history.index,0);
  // 下一次返回照常交给路由。
  history.pushState(null,'','/#other');history.back();await settle();
  assert.deepEqual(routed,['/#chat']);
});

test('closing with a button pops the pushed entry without routing',async()=>{
  reset('/#chat');
  history.pushState({chatRunOrigin:{taskId:'t'}},'','/#chat');
  const dialog=new HTMLDialogElement();dialog.showModal();
  assert.deepEqual(history.state.chatRunOrigin,{taskId:'t'});
  dialog.close();await settle();
  assert.equal(history.index,1);
  assert.equal(history.state.bridgeModal,undefined);
  assert.deepEqual(history.state.chatRunOrigin,{taskId:'t'});
  assert.deepEqual(routed,[]);
});

test('nested dialogs close one per back',async()=>{
  reset();
  const outer=new HTMLDialogElement(),inner=new HTMLDialogElement();
  outer.showModal();inner.showModal();
  history.back();await settle();
  assert.equal(inner.open,false);assert.equal(outer.open,true);
  history.back();await settle();
  assert.equal(outer.open,false);
  assert.equal(history.index,0);assert.deepEqual(routed,[]);
});

test('a busy dialog that refuses cancel stays open and keeps its entry',async()=>{
  reset();
  const dialog=new HTMLDialogElement();
  dialog.addEventListener('cancel',event=>event.preventDefault());
  dialog.showModal();
  history.back();await settle();
  assert.equal(dialog.open,true);
  assert.equal(typeof history.state.bridgeModal,'string');
  dialog.listeners.cancel=[];
  history.back();await settle();
  assert.equal(dialog.open,false);assert.equal(history.index,0);assert.deepEqual(routed,[]);
});

test('navigating from a dialog keeps the new route and skips the dead entry later',async()=>{
  reset('/');
  const dialog=new HTMLDialogElement();dialog.showModal();
  // 如待处理列表：先关弹窗，同步压入聊天路由，close 事件随后才到。
  dialog.close();history.pushState(null,'','/#chat');
  await settle();
  assert.equal(history.entries[history.index].url,'/#chat');
  history.back();await settle();
  assert.equal(history.index,0);
  assert.deepEqual(routed,['/']);
});

test('dialogs opened without user activation do not touch history',async()=>{
  reset();
  navigator.userActivation.isActive=false;
  const dialog=new HTMLDialogElement();dialog.showModal();
  assert.equal(history.entries.length,1);
  dialog.close();await settle();
  assert.equal(history.entries.length,1);
});
