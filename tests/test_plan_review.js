'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const review=require('../web/plan-review.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.className='';this.textContent='';this.dataset={};}
  append(...nodes){this.children.push(...nodes);}
  setAttribute(name,value){this[name]=value;}
}
const params=()=>({
  planId:'plan-1',planVersion:2,planHash:'a'.repeat(64),
  previousPlanContent:'# 计划\n\n第一步\n第二步',planContent:'# 计划\n\n第一步\n第二步\n测试',
});

test('无版本元数据的旧运行时反馈保持原样',()=>{
  assert.equal(review.formatFeedback('补充错误处理',{}),'补充错误处理');
});

test('修订反馈绑定计划、版本和完整内容哈希',()=>{
  const text=review.formatFeedback('> 第一步\n\n补充错误处理',params());
  assert.ok(text.includes('Plan-ID: "plan-1"'));
  assert.ok(text.includes('Plan-Version: 2'));
  assert.ok(text.includes('Plan-SHA256: '+'a'.repeat(64)));
  assert.ok(text.includes('Do not resubmit the plan unchanged.'));
  assert.ok(text.endsWith('> 第一步\n\n补充错误处理'));
});

test('不完整或非法版本元数据不进入反馈协议',()=>{
  assert.equal(review.formatFeedback('意见',{planId:'plan-1',planVersion:2,planHash:'abc'}),'意见');
  assert.equal(review.metadata({...params(),planVersion:0}),null);
});

test('长段未变更行折叠且保留上下文与变更顺序',()=>{
  const rows=Array.from({length:12},(_,index)=>({kind:'context',text:String(index)}));
  rows.push({kind:'add',text:'new'});
  const segments=review.foldRows(rows);
  assert.deepEqual(segments.map(segment=>segment.kind),['rows','fold','rows','rows']);
  assert.equal(segments[1].rows.length,8);
  assert.equal(segments.flatMap(segment=>segment.rows).length,rows.length);
  assert.equal(segments.at(-1).rows[0].kind,'add');
});

test('修订卡片展示增删统计和原始行级差异',()=>{
  global.window={};
  require('../web/diff.js');
  global.document={createElement:tag=>new FakeNode(tag)};
  const details=review.diffDetails(params());
  assert.ok(details);
  assert.equal(details.tag,'details');
  assert.equal(details.children[0].children[1].textContent,'+1');
  assert.equal(details.children[0].children[3].textContent,'−0');
  const view=details.children.at(-1);
  assert.ok(view.children.some(node=>node.className==='diff-line add'));
});

test('相同正文不产生修订差异入口',()=>{
  global.window={BridgeDiff:{pairRows:()=>({rows:[],degraded:false})}};
  global.document={createElement:tag=>new FakeNode(tag)};
  const value={...params(),previousPlanContent:params().planContent};
  assert.equal(review.diffDetails(value),null);
});

test('历史版本只在展开时加载并可与当前版本比较',async()=>{
  global.window={};delete require.cache[require.resolve('../web/diff.js')];require('../web/diff.js');
  global.document={createElement:tag=>new FakeNode(tag)};
  let calls=0;
  const details=review.historyDetails(params(),async()=>{calls++;return {planId:'plan-1',revisions:[
    {version:1,hash:'b'.repeat(64),content:'# 旧版\n第一步',createdAt:'2026-11-24T10:00:00Z'},
    {version:2,hash:'a'.repeat(64),content:params().planContent,createdAt:'2026-11-24T11:00:00Z'},
  ]};},(node,text)=>{node.textContent=text;});
  assert.equal(calls,0);
  details.open=true;await details.ontoggle();
  assert.equal(calls,1);
  const list=details.children[2];
  assert.equal(list.children.length,2);
  assert.equal(list.children[0].children[0].textContent.startsWith('版本 2'),true);
  const old=list.children[1];old.open=true;old.ontoggle();
  assert.equal(old.children[1].textContent,'# 旧版\n第一步');
  assert.equal(old.children[2].className,'plan-revision-diff');
  await details.ontoggle();assert.equal(calls,1);
});

test('历史加载失败后可收起重试',async()=>{
  global.window={};delete require.cache[require.resolve('../web/diff.js')];require('../web/diff.js');
  global.document={createElement:tag=>new FakeNode(tag)};
  let calls=0;
  const details=review.historyDetails(params(),async()=>{calls++;if(calls===1)throw Error('网络中断');return {planId:'plan-1',revisions:[]};});
  details.open=true;await details.ontoggle();
  assert.equal(details.children[1].className,'error');
  assert.match(details.children[1].textContent,/网络中断/);
  details.open=false;details.open=true;await details.ontoggle();
  assert.equal(calls,2);
  assert.equal(details.children[1].textContent,'暂无历史版本');
});

test('历史响应必须匹配计划且版本字段完整',async()=>{
  global.window={};delete require.cache[require.resolve('../web/diff.js')];require('../web/diff.js');
  global.document={createElement:tag=>new FakeNode(tag)};
  const wrong=review.historyDetails(params(),async()=>({planId:'other',revisions:[]}));
  wrong.open=true;await wrong.ontoggle();
  assert.match(wrong.children[1].textContent,/历史版本数据无效/);
  const invalid=review.historyDetails(params(),async()=>({planId:'plan-1',revisions:[{version:1,hash:'bad',content:'x'}]}));
  invalid.open=true;await invalid.ontoggle();
  assert.match(invalid.children[1].textContent,/历史版本数据无效/);
});
