'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode {
  constructor(tag){this.tag=tag;this.children=[];this.className='';this.textContent='';}
  append(...nodes){this.children.push(...nodes);}
}

function loadBadge(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function approvalRisk'),end=source.indexOf('function markRecommendation',start);
  const elStart=source.indexOf('function el('),elEnd=source.indexOf('\n',elStart);
  const context={
    document:{createElement:tag=>new FakeNode(tag),createDocumentFragment:()=>new FakeNode('#fragment')},
    t:value=>value,
  };
  vm.runInNewContext(source.slice(elStart,elEnd)+'\n'+source.slice(start,end)+'\nthis.riskBadge=riskBadge;',context);
  return context.riskBadge;
}

const params={command:'cat tests/test_migrate.js | head -80'};

test('cards Jev never assessed keep the local label only',()=>{
  const badge=loadBadge()({id:'r'},params);
  assert.equal(badge.className,'approval-risk command');
  assert.equal(badge.textContent,'命令执行');
});

test('an assessed card shows the Jev verdict and why it was held',()=>{
  const badge=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'routine',confidence:0.59,reversible:0.96,reason:'low_confidence'}},params);
  assert.equal(badge.tag,'#fragment');
  const [risk,reason]=badge.children;
  assert.equal(risk.className,'approval-risk risk-routine');
  assert.equal(risk.textContent,'常规操作 · Jev 59% · 可撤销');
  assert.equal(reason.textContent,'未自动审核：置信度 59% < 80%');
});

test('threshold holds show both numbers',()=>{
  const badge=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'elevated',confidence:0.18,reversible:0.93,reason:'recommend_review'}},params);
  assert.equal(badge.children[1].textContent,'未自动审核：Jev 建议人工查看');
  const over=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'elevated',confidence:0.9,reversible:0.9,negativeSum:0.41,threshold:0.3,reason:'negative_sum_over_threshold'}},params);
  assert.equal(over.children[1].textContent,'未自动审核：风险倾向 41% ≥ 阈值 30%');
});
