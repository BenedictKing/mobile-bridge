'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode {
  constructor(tag){this.tag=tag;this.children=[];this.className='';this.textContent='';}
  append(...nodes){this.children.push(...nodes);}
}

function loadApprovalHelpers(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const start=source.indexOf('function approvalRisk'),end=source.indexOf('function renderApprovals',start);
  const elStart=source.indexOf('function el('),elEnd=source.indexOf('\n',elStart);
  const context={
    document:{createElement:tag=>new FakeNode(tag),createDocumentFragment:()=>new FakeNode('#fragment')},
    t:value=>value,
  };
  vm.runInNewContext(source.slice(elStart,elEnd)+'\n'+source.slice(start,end)+'\nthis.riskBadge=riskBadge;this.orderedApprovals=orderedApprovals;',context);
  return context;
}
const loadBadge=()=>loadApprovalHelpers().riskBadge;
const loadOrder=()=>loadApprovalHelpers().orderedApprovals;

const params={command:'cat tests/test_migrate.js | head -80'};

test('cards Jev never assessed keep the local label only',()=>{
  const badge=loadBadge()({id:'r'},params);
  assert.equal(badge.className,'approval-risk command');
  assert.equal(badge.textContent,'命令执行');
});

test('an assessed card shows the Jev verdict and why it was held',()=>{
  // reason 必须是审批决策流（internal/jev/approval.go）真实会发的值；
  // low_confidence 属于自动续跑门槛（internal/jev/continue.go），到不了审批卡。
  const badge=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'routine',confidence:0.59,reversible:0.4,reason:'not_reversible_enough'}},params);
  assert.equal(badge.tag,'#fragment');
  const [risk,reason]=badge.children;
  assert.equal(risk.className,'approval-risk risk-routine');
  assert.equal(risk.textContent,'常规操作 · Jev 59% · 难以撤销');
  assert.equal(reason.textContent,'未自动审核：可撤销性 40% < 80%');
});

test('a failed Jev assessment leaves an explicit manual-fallback trace',()=>{
  const badge=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'unknown',reason:'assess_failed'}},params);
  assert.equal(badge.tag,'#fragment');
  const [risk,reason]=badge.children;
  assert.equal(risk.textContent,'Jev 评估失败');
  assert.equal(reason.textContent,'未自动审核：已转人工确认');
});

test('a failed assessment still being retried invites a manual answer',()=>{
  const badge=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'unknown',reason:'assess_failed',retrying:true,attempt:0}},params);
  const [risk,reason]=badge.children;
  assert.equal(risk.textContent,'Jev 评估超时，正在重试');
  assert.equal(reason.textContent,'可直接手动确认，不必等待');
});

test('threshold holds show both numbers',()=>{
  const badge=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'elevated',confidence:0.18,reversible:0.93,reason:'recommend_review'}},params);
  assert.equal(badge.children[1].textContent,'未自动审核：Jev 建议人工查看');
  const over=loadBadge()({id:'r',assessment:{source:'jev',riskLabel:'elevated',confidence:0.9,reversible:0.9,negativeSum:0.41,threshold:0.3,reason:'negative_sum_over_threshold'}},params);
  assert.equal(over.children[1].textContent,'未自动审核：风险倾向 41% ≥ 阈值 30%');
});

test('Jev cards sort by risk without mutating requests or breaking object identity',()=>{
  const request=(id,riskLabel)=>({id,recommend:'accept',...(riskLabel?{assessment:{source:'jev',riskLabel}}:{})});
  const requests=[
    request('unassessed'),request('low-1','low'),request('high-1','high'),
    request('elevated','elevated'),request('high-2','high'),request('routine','routine'),
    request('failed','unknown'),request('low-2','low'),
  ];
  const ordered=loadOrder()(requests);
  assert.deepEqual(ordered.map(item=>item.id),['high-1','high-2','elevated','low-1','low-2','routine','unassessed','failed']);
  assert.deepEqual(requests.map(item=>item.id),['unassessed','low-1','high-1','elevated','high-2','routine','failed','low-2']);
  assert.equal(ordered[0],requests[2]);
  assert.equal(ordered[0].recommend,'accept');
});
