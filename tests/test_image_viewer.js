'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const path=require('node:path');

class Node {
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.attributes={};this.listeners={};this.className='';this.textContent='';this.open=false;}
  append(...nodes){this.children.push(...nodes);}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(kind,handler){(this.listeners[kind]??=[]).push(handler);}
  replaceChildren(...nodes){this.children=nodes;}
  showModal(){this.open=true;}
  close(){this.open=false;}
  querySelector(){return null;}
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
