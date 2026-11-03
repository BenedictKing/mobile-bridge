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
