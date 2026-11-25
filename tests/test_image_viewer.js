'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const path=require('node:path');

class Node {
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.attributes={};this.listeners={};this.className='';this.textContent='';this.open=false;this.style={};
    const classes=new Set();this.classList={toggle:(name,on)=>on?classes.add(name):classes.delete(name),contains:name=>classes.has(name)};}
  append(...nodes){this.children.push(...nodes);}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(kind,handler){(this.listeners[kind]??=[]).push(handler);}
  replaceChildren(...nodes){this.children=nodes;}
  showModal(){this.open=true;}
  close(){this.open=false;for(const handler of this.listeners.close||[])handler();}
  querySelector(selector){return this.children.find(node=>node.tag===selector)||null;}
  // Layout box 100x100 at (0,0); the rect follows the viewer's translate+scale.
  getBoundingClientRect(){
    const match=/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([-\d.e]+)\)/.exec(this.style.transform||'');
    const [x,y,scale]=match?match.slice(1).map(Number):[0,0,1];
    return {left:50+x-50*scale,top:50+y-50*scale,width:100*scale,height:100*scale};
  }
}

global.location={href:'http://localhost/'};
global.BridgeConnection={trusted:()=>true};
global.document={
  body:new Node('body'),
  listeners:{},
  createElement:tag=>new Node(tag),
  createElementNS:(ns,tag)=>new Node(tag),
  addEventListener(kind,handler){(this.listeners[kind]??=[]).push(handler);},
};
global.BridgeIcons=require('../web/icons.js');
require(path.join(__dirname,'..','web','image-viewer.js'));

function openPreview(){
  const target=new Node('a');
  target.dataset.imagePreview='/api/sessions/sess/files/'+('a'.repeat(64));
  target.dataset.imageName='preview.png';
  target.closest=()=>target;
  const event={target,button:0,preventDefault(){}};
  for(const handler of document.listeners.click)handler(event);
  return document.body.children.find(node=>node.id==='image-preview-dialog');
}

test('clicking empty image content closes the preview',()=>{
  const dialog=openPreview();
  const content=dialog.children[1];
  assert.equal(dialog.open,true);
  for(const handler of dialog.listeners.click)handler({target:content});
  assert.equal(dialog.open,false);
});

test('clicking the image does not close the preview',()=>{
  const dialog=openPreview();
  const content=dialog.children[1],image=content.children[0];
  for(const handler of dialog.listeners.click)handler({target:image});
  assert.equal(dialog.open,true);
  for(const handler of dialog.listeners.click)handler({target:dialog});
  assert.equal(dialog.open,false);
});

test('ready composer images expose the original preview URL',()=>{
  global.BridgeI18n={t:value=>value};
  const {ChatAttachments}=require('../web/attachments.js');
  const root=new Node('div');
  const attachments=new ChatAttachments({root,button:new Node('button'),input:new Node('input'),upload:async()=>({}),
    preview:(key,id)=>'/api/sessions/sess/uploads/'+id+'/preview?host=local'});
  const id='a'.repeat(36);
  attachments.key='local|sess';
  attachments.rows=[{id,name:'draft.png',size:1,image:true,thumb:'image/webp',status:'ready'}];
  attachments.render();
  const image=root.children[0].children[0];
  assert.equal(image.dataset.imagePreview,'/api/sessions/sess/uploads/'+id+'/preview?host=local&variant=original');
  assert.equal(image.dataset.imageName,'draft.png');
  assert.equal(image.attributes.role,'button');
  assert.equal(image.tabIndex,0);
});

function fire(node,kind,event){for(const handler of node.listeners[kind]||[])handler({preventDefault(){},timeStamp:0,...event});}
function gestureSetup(){
  const dialog=openPreview(),content=dialog.children[1],image=content.children[0];
  return {dialog,content,image,view:()=>{
    const match=/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([-\d.e]+)\)/.exec(image.style.transform||'');
    return match?match.slice(1).map(Number):[0,0,1];
  }};
}

test('pinch zooms around the fingers and lifting on the backdrop does not dismiss',()=>{
  const {dialog,content,image,view}=gestureSetup();
  fire(content,'pointerdown',{pointerId:1,pointerType:'touch',target:image,clientX:40,clientY:50});
  fire(content,'pointerdown',{pointerId:2,pointerType:'touch',target:image,clientX:60,clientY:50});
  fire(content,'pointermove',{pointerId:1,pointerType:'touch',target:image,clientX:30,clientY:50});
  fire(content,'pointermove',{pointerId:2,pointerType:'touch',target:image,clientX:70,clientY:50});
  const [x,y,scale]=view();
  assert.equal(scale,2);assert.equal(x,0);assert.equal(y,0);
  assert.equal(content.classList.contains('zoomed'),true);
  fire(content,'pointerup',{pointerId:1,pointerType:'touch',target:content,clientX:30,clientY:50});
  fire(content,'pointerup',{pointerId:2,pointerType:'touch',target:content,clientX:70,clientY:50});
  fire(dialog,'click',{target:content});
  assert.equal(dialog.open,true);
  // The swallow is one-shot: a later plain tap on empty space still closes.
  fire(content,'pointerdown',{pointerId:3,pointerType:'touch',target:content,clientX:5,clientY:5});
  fire(content,'pointerup',{pointerId:3,pointerType:'touch',target:content,clientX:5,clientY:5});
  fire(dialog,'click',{target:content});
  assert.equal(dialog.open,false);
  assert.equal(content.classList.contains('zoomed'),false);
  assert.equal(content.children.length,0);
});

test('one-finger drag pans a zoomed image but stays within its bounds',()=>{
  const {content,image,view}=gestureSetup();
  fire(content,'wheel',{target:image,clientX:50,clientY:50,deltaY:-100*Math.log(2),ctrlKey:true});
  assert.ok(Math.abs(view()[2]-2)<1e-9);
  fire(content,'pointerdown',{pointerId:1,pointerType:'touch',target:image,clientX:50,clientY:50});
  fire(content,'pointermove',{pointerId:1,pointerType:'touch',target:image,clientX:70,clientY:40});
  let [x,y]=view();assert.ok(Math.abs(x-20)<1e-9&&Math.abs(y+10)<1e-9);
  fire(content,'pointermove',{pointerId:1,pointerType:'touch',target:image,clientX:500,clientY:50});
  [x]=view();assert.ok(Math.abs(x-50)<1e-9);
});

test('drag on an unzoomed image does nothing and double tap toggles zoom',()=>{
  const {content,image,view}=gestureSetup();
  fire(content,'pointerdown',{pointerId:1,pointerType:'touch',target:image,clientX:50,clientY:50});
  fire(content,'pointermove',{pointerId:1,pointerType:'touch',target:image,clientX:90,clientY:50});
  assert.equal(image.style.transform,'');
  fire(content,'pointerup',{pointerId:1,pointerType:'touch',target:image,clientX:90,clientY:50});
  for(const t of [1000,1200]){
    fire(content,'pointerdown',{pointerId:2,pointerType:'touch',target:image,clientX:50,clientY:50,timeStamp:t});
    fire(content,'pointerup',{pointerId:2,pointerType:'touch',target:image,clientX:50,clientY:50,timeStamp:t});
  }
  assert.equal(view()[2],2.5);
  for(const t of [2000,2200]){
    fire(content,'pointerdown',{pointerId:3,pointerType:'touch',target:image,clientX:50,clientY:50,timeStamp:t});
    fire(content,'pointerup',{pointerId:3,pointerType:'touch',target:image,clientX:50,clientY:50,timeStamp:t});
  }
  assert.equal(image.style.transform,'');
});
