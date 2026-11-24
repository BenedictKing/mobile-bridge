'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const review=require('../web/plan-review.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.className='';this.textContent='';this.dataset={};}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
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

class MemoryStore{constructor(){this.map=new Map();}getItem(key){return this.map.has(key)?this.map.get(key):null;}setItem(key,value){this.map.set(key,String(value));}removeItem(key){this.map.delete(key);}}
const textarea=()=>Object.assign(new FakeNode('textarea'),{value:'',focus(){this.focused=true;}});
function withDom(){global.window={};global.document={createElement:tag=>new FakeNode(tag)};}

test('结构化批注按编号列出原文、类型与全局意见',()=>{
  const text=review.formatFeedback('整体再精简',params(),[
    {id:'A1',type:'comment',quote:'第一步\n第二步',body:'拆成两个任务'},
    {id:'A2',type:'delete',quote:'测试',body:'重复'},
    {id:'bad',type:'comment',quote:'x',body:'忽略'},
  ]);
  assert.ok(text.startsWith('## Plan revision feedback'));
  assert.ok(text.includes('#### A1 · Comment\n\n> 第一步\n> 第二步\n\n拆成两个任务'));
  assert.ok(text.includes('#### A2 · Delete\n\n> 测试\n\nRemove the quoted content. Reason: 重复'));
  assert.ok(text.endsWith('### General feedback\n\n整体再精简'));
  assert.equal(text.includes('忽略'),false);
  // 无版本元数据时同样结构化，只是不带版本头。
  const legacy=review.formatFeedback('',{},[{id:'A1',type:'delete',quote:'测试'}]);
  assert.ok(legacy.startsWith('### Annotations'));
  assert.equal(legacy.includes('General feedback'),false);
});

test('批注编号单调递增、校验必填并在成功后清除草稿',()=>{
  withDom();const store=new MemoryStore();global.sessionStorage=store;
  const input=textarea();
  let changes=0;
  const editor=review.reviewer({root:new FakeNode('div'),textarea:input,params:params(),storageKey:'k',onChange:()=>changes++});
  assert.equal(editor.element.hidden,true);
  assert.equal(editor.validate(''),'请填写修改意见');
  editor.add('comment',{text:'第一步',truncated:false});
  editor.add('delete',{text:'第二步',truncated:false});
  assert.equal(editor.element.hidden,false);
  assert.equal(changes,2);
  assert.match(editor.validate(''),/A1/);
  const list=editor.element.children[1];
  const first=list.children[0].children[2];first.value='改写';first.oninput();
  assert.equal(editor.validate(''),'');
  // 移除 A1 后新批注继续用 A3，避免与已发出的编号混淆。
  list.children[0].children[0].children[1].onclick();
  editor.add('comment',{text:'测试',truncated:false});
  assert.deepEqual(editor.payload('全局').annotations.map(item=>item.id),['A2','A3']);
  assert.deepEqual(JSON.parse(store.getItem('k')).annotations.map(item=>item.id),['A2','A3']);
  editor.clear();
  assert.equal(store.getItem('k'),null);
  assert.equal(editor.count(),0);
  delete global.sessionStorage;
});

test('批注草稿在卡片重绘后恢复且忽略非法条目',()=>{
  withDom();const store=new MemoryStore();global.sessionStorage=store;
  store.setItem('k',JSON.stringify({text:'全局草稿',annotations:[{id:'A1',type:'comment',quote:'第一步',body:'保留'},{id:'A2',type:'replace',quote:'x'},{id:'A3',type:'delete',quote:'  '}]}));
  const input=textarea(),editor=review.reviewer({root:new FakeNode('div'),textarea:input,params:params(),storageKey:'k'});
  assert.equal(input.value,'全局草稿');
  assert.deepEqual(editor.payload(input.value),{text:'全局草稿',annotations:[{id:'A1',type:'comment',quote:'第一步',body:'保留'}]});
  store.setItem('broken','{');
  const fresh=review.reviewer({root:new FakeNode('div'),textarea:textarea(),params:params(),storageKey:'broken'});
  assert.equal(fresh.count(),0);
  delete global.sessionStorage;
});

test('超出请求体预算的批注被拦截而不是提交',()=>{
  withDom();
  const editor=review.reviewer({root:new FakeNode('div'),textarea:textarea(),params:params()});
  for(let index=0;index<12;index++){
    editor.add('comment',{text:'引'.repeat(4000),truncated:false});
    const row=editor.element.children[1].children.at(-1),input=row.children[2];input.value='意'.repeat(8000);input.oninput();
  }
  assert.equal(editor.validate(''),'批注内容过长，请精简后再提交');
});

test('锚点按空白归一匹配并忽略截断标记',()=>{
  assert.equal(review.anchorFound('第一步\n  第二步','# 计划 第一步 第二步 测试'),true);
  assert.equal(review.anchorFound('第一步\n…','第一步继续'),true);
  assert.equal(review.anchorFound('已删除的段落','第一步'),false);
  assert.equal(review.anchorFound('  ','第一步'),false);
  // 列表项的 textContent 之间可能没有空白，选区文本却带换行。
  assert.equal(review.anchorFound('第一项\n第二项','第一项第二项'),true);
});

test('上一版批注显示锚点状态并与历史共用一次加载',async()=>{
  withDom();
  let calls=0;
  const load=review.memo(async()=>{calls++;return {planId:'plan-1',revisions:[
    {version:1,hash:'b'.repeat(64),content:'旧',createdAt:'2026-11-24T10:00:00Z',feedback:[{text:'整体精简',annotations:[
      {id:'A1',type:'comment',quote:'第一步',body:'细化'},{id:'A2',type:'delete',quote:'已删除的段落',body:''}]}]},
    {version:2,hash:'a'.repeat(64),content:params().planContent,createdAt:'2026-11-24T11:00:00Z'},
  ]};});
  assert.equal(review.previousFeedbackDetails({...params(),planVersion:1},load),null);
  const panel=review.previousFeedbackDetails(params(),load,()=>'计划 第一步 第二步');
  const history=review.historyDetails(params(),load,(node,text)=>{node.textContent=text;});
  panel.open=true;await panel.ontoggle();
  history.open=true;await history.ontoggle();
  assert.equal(calls,1);
  const rows=panel.children[2].children;
  assert.equal(rows[0].children[0].children[1].className,'plan-anchor-status present');
  assert.equal(rows[1].children[0].children[1].className,'plan-anchor-status stale');
  assert.equal(rows[1].children[0].children[1].textContent,'原文已修改或移除');
  assert.equal(rows[2].textContent,'全局意见：整体精简');
  const old=history.children[2].children[1];old.open=true;old.ontoggle();
  assert.equal(old.children.at(-2).textContent,'该版收到的反馈');
});

test('上一版无批注时明确提示，加载失败可重试',async()=>{
  withDom();
  let calls=0;
  const load=review.memo(async()=>{calls++;if(calls===1)throw Error('断网');return {planId:'plan-1',revisions:[{version:1,hash:'b'.repeat(64),content:'旧'}]};});
  const panel=review.previousFeedbackDetails(params(),load);
  panel.open=true;await panel.ontoggle();
  assert.match(panel.children[1].textContent,/断网/);
  await panel.ontoggle();
  assert.equal(calls,2);
  assert.equal(panel.children[1].textContent,'上一版没有记录批注');
});
