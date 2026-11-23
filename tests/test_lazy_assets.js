'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {createLazyLoader}=require('../web/lazy.js');

function harness(manifest={},failures=[]){
  const requests=[],nodes=[];
  const document={
    getElementById:id=>id==='bridge-assets'?{textContent:JSON.stringify(manifest)}:null,
    createElement:tag=>({tag,dataset:{},remove(){this.removed=true;}}),
    head:{append(node){
      requests.push(node.dataset.bridgeLazy);nodes.push(node);
      const path=node.dataset.bridgeLazy;
      queueMicrotask(()=>{
        const count=requests.filter(value=>value===path).length;
        if(failures.includes(path)&&count===1)node.onerror();
        else node.onload();
      });
    }},
  };
  return {loader:createLazyLoader(document),requests,nodes};
}

test('lazy groups do not request assets until their entry is used',async()=>{
  const {loader,requests}=harness();
  assert.deepEqual(requests,[]);
  await loader.load('accounts');
  assert.deepEqual(requests,['/account.js','/accounts.js','/pool.js']);
});

test('production manifest maps deferred assets to hashed URLs',async()=>{
  const {loader,nodes}=harness({'/account.js':'/account.0123456789ab.js'});
  await loader.load('accounts');
  assert.equal(nodes[0].src,'/account.0123456789ab.js');
  assert.equal(nodes[0].async,false);
});

test('concurrent entry calls share one group load',async()=>{
  const {loader,requests}=harness();
  const first=loader.load('graph'),second=loader.load('graph');
  assert.equal(first,second);
  await Promise.all([first,second]);
  assert.deepEqual(requests,['/vendor/dagre.min.js']);
});

test('markdown dependencies finish before the renderer and styles load in parallel',async()=>{
  const {loader,requests}=harness();
  await loader.load('markdown');
  assert.deepEqual(requests.slice(0,4),[
    '/vendor/katex/katex.min.css','/vendor/markdown-it.min.js','/vendor/katex/katex.min.js','/vendor/texmath.js',
  ]);
  assert.equal(requests[4],'/markdown.js');
});

test('a failed final resource retries without downloading successful dependencies again',async()=>{
  const {loader,requests,nodes}=harness({},['/markdown.js']);
  await assert.rejects(loader.load('markdown'),/markdown\.js/);
  assert.equal(nodes.at(-1).removed,true);
  await loader.load('markdown');
  assert.equal(requests.filter(path=>path==='/markdown.js').length,2);
  assert.equal(requests.filter(path=>path==='/vendor/katex/katex.min.js').length,1);
});

test('unknown groups fail without side effects',async()=>{
  const {loader,requests}=harness();
  await assert.rejects(loader.load('missing'),/未知延迟资源组/);
  assert.deepEqual(requests,[]);
});
