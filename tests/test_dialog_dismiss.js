'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {closeOnBackdrop,bindDeclared}=require('../web/dialog-dismiss.js');

// 面板矩形：左上 (10,300)，右下 (380,800)；.picker 的内边距也在矩形内。
function fakeDialog(){
  const listeners={};
  const dialog={closed:0,dataset:{},
    addEventListener:(type,fn)=>{(listeners[type]??=[]).push(fn);},
    fire:(type,event={})=>{for(const fn of listeners[type]||[])fn({target:dialog,...event});},
    close(){this.closed++;this.fire('close');},
    getBoundingClientRect:()=>({left:10,top:300,right:380,bottom:800})};
  return dialog;
}
const OUT={clientX:200,clientY:100},PADDING={clientX:20,clientY:310};
function tap(dialog,point,target=dialog){dialog.fire('pointerdown',{...point,target});dialog.fire('click',{...point,target});}

test('按下与松开都在面板外才关闭',()=>{
  const dialog=fakeDialog();closeOnBackdrop(dialog);
  tap(dialog,OUT);
  assert.equal(dialog.closed,1);
});

test('点面板内边距或面板内容不关闭',()=>{
  const dialog=fakeDialog();closeOnBackdrop(dialog);
  tap(dialog,PADDING);
  tap(dialog,OUT,{});
  assert.equal(dialog.closed,0);
});

test('从面板内拖选文字到面板外松开不关闭',()=>{
  const dialog=fakeDialog();closeOnBackdrop(dialog);
  dialog.fire('pointerdown',{...PADDING,target:{}});
  dialog.fire('click',OUT);
  assert.equal(dialog.closed,0);
});

test('在多行文本框里打过字后不关闭，关闭后重新计算',()=>{
  const dialog=fakeDialog();closeOnBackdrop(dialog);
  dialog.fire('input',{target:{tagName:'SELECT'}});
  dialog.fire('input',{target:{tagName:'TEXTAREA'}});
  tap(dialog,OUT);
  assert.equal(dialog.closed,0);
  dialog.close();
  tap(dialog,OUT);
  assert.equal(dialog.closed,2);
});

test('声明 data-backdrop-close 的弹窗各绑定一次',()=>{
  const a=fakeDialog(),b=fakeDialog();
  const root={querySelectorAll:selector=>{assert.equal(selector,'dialog[data-backdrop-close]');return [a,b];}};
  bindDeclared(root);bindDeclared(root);
  tap(a,OUT);tap(b,OUT);
  assert.deepEqual([a.closed,b.closed],[1,1]);
});
