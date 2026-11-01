'use strict';
// selection.js 的浮条行为：这里用一个最小假 DOM 驱动真实模块，
// 覆盖「出现/不出现/点击后写入输入框」三条主路径，不依赖浏览器。
const test=require('node:test');
const assert=require('node:assert/strict');
const BridgeSelection=require('../web/selection.js');

class FakeElement{
  constructor(tag){
    this.tag=tag;this.tagName=tag.toUpperCase();this.children=[];this.parentElement=null;
    this.className='';this.dataset={};this.style={};this.hidden=false;this.textContent='';
    this.offsetWidth=60;this.offsetHeight=36;this.readOnly=false;this.disabled=false;
    this.value='';this.handlers=new Map();
  }
  append(...nodes){for(const node of nodes){if(node&&typeof node==='object')node.parentElement=this;this.children.push(node);}}
  contains(node){for(let cur=node;cur;cur=cur.parentElement||null)if(cur===this)return true;return false;}
  closest(selector){
    for(const part of selector.split(',')){
      const sel=part.trim();
      for(let cur=this;cur;cur=cur.parentElement||null)if(matches(cur,sel))return cur;
    }
    return null;
  }
  addEventListener(type,handler){if(!this.handlers.has(type))this.handlers.set(type,[]);this.handlers.get(type).push(handler);}
  setAttribute(name,value){this.attributes={...this.attributes,[name]:String(value)};}
  getAttribute(name){return this.attributes?.[name]??null;}
  fire(type,event={}){for(const handler of this.handlers.get(type)||[])handler(event);}
  dispatchEvent(event){this.fire(event.type,event);return true;}
  focus(){this.focused=true;}
  setSelectionRange(start,end){this.selection={start,end};}
  scrollIntoView(){this.scrolled=true;}
  getBoundingClientRect(){return {left:0,top:0,width:0,height:0,right:0,bottom:0};}
}
function matches(node,sel){
  if(sel.startsWith('.'))return node.className.split(/\s+/).includes(sel.slice(1));
  if(sel.startsWith('[')){
    const name=sel.slice(1,-1);
    const key=name.startsWith('data-')?name.slice(5).replace(/-([a-z])/g,(_,letter)=>letter.toUpperCase()):name;
    return key in node.dataset;
  }
  return node.tag===sel;
}
const sleep=ms=>new Promise(resolve=>setTimeout(resolve,ms));

function setup({sameRow=true,insideButton=false,readOnly=false}={}){
  const body=new FakeElement('body');
  const messages=new FakeElement('div');
  const row=(key)=>{
    const element=new FakeElement('div');
    element.className='message timeline-row assistant';element.dataset.key=key;
    const content=new FakeElement('div');content.className='message-body';
    const text={nodeType:3,parentElement:content,textContent:'原文内容'};
    content.append(text);element.append(content);
    messages.append(element);
    return {element,content,text};
  };
  const first=row('r1'),second=row('r2');
  let start=first.text,end=(sameRow?first:second).text;
  if(insideButton){
    const button=new FakeElement('button');button.className='message-copy';button.textContent='复制';
    const label={nodeType:3,parentElement:button,textContent:'复制'};
    button.append(label);first.element.append(button);
    start=label;end=label;
  }
  const range={
    startContainer:start,endContainer:end,
    getBoundingClientRect:()=>({left:40,top:200,width:120,height:18,right:160,bottom:218})
  };
  const selection={
    isCollapsed:false,rangeCount:1,
    toString:()=>'原文内容',
    getRangeAt:()=>range,
    removeAllRanges(){this.isCollapsed=true;}
  };
  const listeners=new Map();
  const document={
    body,createElement:tag=>new FakeElement(tag),getElementById:()=>null,
    addEventListener(type,handler){if(!listeners.has(type))listeners.set(type,[]);listeners.get(type).push(handler);},
    emit(type,event={}){for(const handler of listeners.get(type)||[])handler(event);}
  };
  const window={innerWidth:390,innerHeight:844,addEventListener(){},getSelection:()=>selection};
  global.document=document;global.window=window;
  const textarea=new FakeElement('textarea');textarea.readOnly=readOnly;
  const notes=[];
  BridgeSelection.attach({root:messages,textarea,enabled:()=>true,notify:message=>notes.push(message)});
  const bar=body.children[body.children.length-1];
  const button=bar.children[0];
  return {document,selection,textarea,bar,button,notes,messages};
}

test('选中同一条消息的文字后浮出「评论」',async()=>{
  const env=setup();
  env.document.emit('selectionchange');
  await sleep(220);
  assert.equal(env.bar.hidden,false);
  assert.equal(env.bar.style.top,'226px');
  assert.equal(env.bar.style.left,'70px');
});

test('跨消息的选区不出现（引用没有唯一归属）',async()=>{
  const env=setup({sameRow:false});
  env.document.emit('selectionchange');
  await sleep(220);
  assert.equal(env.bar.hidden,true);
});

test('选中操作栏按钮的文字不出现',async()=>{
  const env=setup({insideButton:true});
  env.document.emit('selectionchange');
  await sleep(220);
  assert.equal(env.bar.hidden,true);
});

test('点「评论」把选区写成引用，并把光标留在引用之后',async()=>{
  const env=setup();
  env.document.emit('selectionchange');
  await sleep(220);
  env.bar.fire('pointerdown');
  env.selection.isCollapsed=true;
  env.document.emit('selectionchange');
  env.bar.fire('click',{target:env.button});
  assert.equal(env.textarea.value,'> 原文内容\n\n');
  assert.equal(env.textarea.selection.start,env.textarea.value.length);
  assert.equal(env.bar.hidden,true);
});

test('已有草稿时引用接在草稿之后',async()=>{
  const env=setup();
  env.textarea.value='先帮我看看这段';
  env.document.emit('selectionchange');
  await sleep(220);
  env.bar.fire('click',{target:env.button});
  assert.equal(env.textarea.value,'先帮我看看这段\n\n> 原文内容\n\n');
});

test('只读会话只提示，不往输入框里塞文字',async()=>{
  const env=setup({readOnly:true});
  env.document.emit('selectionchange');
  await sleep(220);
  env.bar.fire('click',{target:env.button});
  assert.equal(env.textarea.value,'');
  assert.deepEqual(env.notes,['这条聊天当前只读，不能引用评论']);
});
