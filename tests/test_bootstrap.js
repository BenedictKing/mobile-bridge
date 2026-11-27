'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {createBootstrap}=require('../web/bootstrap.js');

function setup(){
  const nodes={},listeners=new Map(),timers=[];
  for(const id of ['bootstrap-status','bootstrap-title','bootstrap-message','bootstrap-retry','login','app']){
    const attributes={};nodes[id]={hidden:id==='login'||id==='app'||id==='bootstrap-retry',setAttribute(name,value){attributes[name]=value;},getAttribute:name=>attributes[name]};
  }
  const win={location:{origin:'https://bridge.test',href:'https://bridge.test/',reload(){this.reloaded=true;}},navigator:{language:'zh-CN'},
    localStorage:{getItem:()=>null},setTimeout(callback){timers.push(callback);return timers.length;},clearTimeout(){},
    addEventListener(name,handler){listeners.set(name,handler);},removeEventListener(name){listeners.delete(name);}};
  const doc={readyState:'complete',getElementById:id=>nodes[id]};
  const bootstrap=createBootstrap(win,doc,100);
  return {nodes,win,bootstrap,timers,error:event=>listeners.get('error')?.(event)};
}

test('startup is visible until the application is ready',()=>{
  const {nodes,bootstrap}=setup();
  assert.equal(nodes['bootstrap-status'].hidden,false);assert.equal(nodes['bootstrap-status'].getAttribute('aria-busy'),'true');
  bootstrap.ready();assert.equal(nodes['bootstrap-status'].hidden,true);assert.equal(nodes['bootstrap-status'].getAttribute('aria-busy'),'false');
  assert.equal(nodes.login.hidden,true);assert.equal(nodes.app.hidden,true);
});

test('same-origin required resource failure shows a safe retry state',()=>{
  const {nodes,win,error}=setup();
  error({target:{tagName:'SCRIPT',src:'https://bridge.test/app.js'},error:{message:'secret stack'}});
  assert.equal(nodes['bootstrap-status'].hidden,false);assert.equal(nodes.login.hidden,true);assert.equal(nodes.app.hidden,true);
  assert.match(nodes['bootstrap-message'].textContent,/重新加载/);assert.doesNotMatch(nodes['bootstrap-message'].textContent,/secret/);
  nodes['bootstrap-retry'].onclick();assert.equal(win.location.reloaded,true);
});

test('external resources do not own startup failure and a late ready still recovers a timeout',()=>{
  const {nodes,bootstrap,timers,error}=setup();
  error({target:{tagName:'SCRIPT',src:'https://cdn.example/app.js'}});assert.equal(nodes['bootstrap-status'].hidden,false);
  assert.equal(nodes['bootstrap-retry'].hidden,true);
  timers[0]();assert.match(nodes['bootstrap-title'].textContent,/超时/);
  bootstrap.ready();assert.equal(nodes['bootstrap-status'].hidden,true);assert.equal(nodes.login.hidden,true);
});

test('attributable same-origin runtime errors fail without exposing exception text',()=>{
  const {nodes,error}=setup();
  error({target:{},filename:'https://bridge.test/app.js',error:{message:'private detail'}});
  assert.equal(nodes['bootstrap-status'].hidden,false);assert.doesNotMatch(nodes['bootstrap-message'].textContent,/private detail/);
});
