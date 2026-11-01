'use strict';
// 视图导航必须真的写浏览器/WebView 历史栈，否则安卓返回手势拿不到可退的条目，
// 会直接退出 App（见 MainActivity 的 OnBackInvokedCallback）。这里用切片加载
// app.js 里的导航函数，验证压栈/替换的时机和返回分支。
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeClassList{
  constructor(names=[]){this.names=new Set(names);}
  add(...values){for(const value of values)this.names.add(value);}
  remove(...values){for(const value of values)this.names.delete(value);}
  contains(value){return this.names.has(value);}
  toggle(value,on){if(on===undefined)on=!this.names.has(value);if(on)this.names.add(value);else this.names.delete(value);return on;}
}
class FakeNode{
  constructor(){this.classList=new FakeClassList();this.hidden=false;}
}

function loadNav({compact=true}={}){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const navStart=source.indexOf('let detailPushed=false;'),navEnd=source.indexOf('function saveDraft(){',navStart);
  const routeStart=source.indexOf('function routeFromLocation(){');
  const routeEnd=source.indexOf("window.addEventListener('popstate'",routeStart);
  assert.ok(navStart>0&&navEnd>navStart,'navigation helpers not found');
  assert.ok(routeStart>0&&routeEnd>routeStart,'routeFromLocation not found');
  const calls=[];
  const nodes={app:new FakeNode(),chat:new FakeNode(),welcome:new FakeNode(),'team-run':new FakeNode()};
  const context={
    location:{hash:'',pathname:'/',search:''},
    history:{
      pushState(state,title,url){calls.push(['push',url]);context.location.hash=url.startsWith('#')?url:'';},
      replaceState(state,title,url){calls.push(['replace',url]);context.location.hash=url.startsWith('#')?url:'';},
      back(){calls.push(['back']);},
    },
    $:id=>nodes[id]||new FakeNode(),
    loadList:async()=>{calls.push(['list']);},
    syncDocumentTitle(){},
    toast(){},
    window:{addEventListener(){},matchMedia:()=>({matches:compact})},
    openRunRoute:'',
  };
  vm.runInNewContext(
    source.slice(navStart,navEnd)+'\n'+source.slice(routeStart,routeEnd)
      +'\nthis.route=routeFromLocation;this.setDetailPushed=value=>{detailPushed=value;};'
      +'this.show=showDetailRoute;this.leave=leaveDetail;',
    context
  );
  context.calls=calls;
  context.reset=()=>{calls.length=0;nodes.app.classList.remove('chat-open');};
  return context;
}

test('list to detail pushes exactly one history entry',()=>{
  const nav=loadNav();
  nav.show('#chat-1~local');
  assert.deepEqual(nav.calls,[['push','#chat-1~local']]);
  assert.equal(nav.location.hash,'#chat-1~local');
});

test('switching inside the detail only replaces the entry',()=>{
  const nav=loadNav();
  nav.show('#chat-1~local');
  nav.reset();
  nav.show('#team-t1');
  assert.deepEqual(nav.calls,[['replace','#team-t1']]);
  assert.equal(nav.location.hash,'#team-t1');
});

test('a deep link landing replaces instead of stacking',()=>{
  const nav=loadNav();
  nav.location.hash='#team-t1';
  nav.show('#chat-1~local');
  assert.deepEqual(nav.calls,[['replace','#chat-1~local']]);
});

test('the wide two-column layout keeps replacing instead of pushing',()=>{
  const nav=loadNav({compact:false});
  nav.show('#chat-1~local');
  assert.deepEqual(nav.calls,[['replace','#chat-1~local']],'desktop keeps the parallel panels intact');
});

test('back button pops the pushed entry when there is one',()=>{
  const nav=loadNav();
  nav.show('#chat-1~local');
  nav.reset();
  nav.leave();
  assert.deepEqual(nav.calls,[['back']],'history.back, not an in-place close');
});

test('back button closes in place when the detail came from a deep link',()=>{
  const nav=loadNav();
  nav.location.hash='#chat-1~local';
  nav.$('app').classList.add('chat-open');
  nav.reset();
  nav.leave();
  assert.deepEqual(nav.calls,[['replace','/'],['list']],'no history.back without a list entry below');
  assert.equal(nav.$('app').classList.contains('chat-open'),false);
  assert.equal(nav.location.hash,'');
});

test('popping back to an empty hash closes the detail view',()=>{
  const nav=loadNav();
  nav.show('#chat-1~local');
  nav.reset();
  nav.$('app').classList.add('chat-open');
  nav.location.hash='';
  nav.setDetailPushed(false);
  nav.route();
  assert.equal(nav.$('app').classList.contains('chat-open'),false);
  assert.deepEqual(nav.calls,[['list']]);
});

test('an unknown hash leaves the list alone',()=>{
  const nav=loadNav();
  nav.location.hash='#a b';
  nav.route();
  assert.deepEqual(nav.calls,[]);
});

test('an unknown hash does not close an open detail either',()=>{
  const nav=loadNav();
  nav.show('#chat-1~local');
  nav.reset();
  nav.$('app').classList.add('chat-open');
  nav.location.hash='#a b';
  nav.route();
  assert.equal(nav.$('app').classList.contains('chat-open'),true);
  assert.deepEqual(nav.calls,[]);
});
