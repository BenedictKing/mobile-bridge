'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const {PendingCommands,MemoryStorage}=require('./helpers/pending_commands.js');

class FakeNode {
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.disabled=false;this.value='';}
  append(...nodes){this.children.push(...nodes);}
  setAttribute(name,value){this[name]=value;}
  focus(){this.focused=true;}
  querySelectorAll(selector){
    const matches=[];
    const visit=node=>{if(selector===node.tag)matches.push(node);for(const child of node.children)visit(child);};
    visit(this);
    return matches;
  }
}

function loadApproval(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8'),calls=[];
  const start=source.indexOf('function renderAcpApproval'),end=source.indexOf('function renderAcpInput',start);
  const elStart=source.indexOf('function el('),elEnd=source.indexOf('\n',elStart);
  const context={
    window:{BridgePendingCommands:new PendingCommands(new MemoryStorage())},uuid:()=> 'command-1',chatKey:(id,host)=>host+'|'+id,
    currentId:'session',currentHost:'local',state:{agent:'kimi'},submittedRequestIds:new Set(),approvalBusy:0,
    document:{createElement:tag=>new FakeNode(tag)},t:value=>value,
    setLanguageBusy(){},sessionUrl:(id,action,host)=>'/api/sessions/'+id+(action?'/'+action:'')+'?host='+encodeURIComponent(host),bumpRecency(){},riskBadge:()=>null,markRecommendation(){},
    renderMarkdown:(node,text)=>{node.renderedMarkdown=text;},
    api:async(path,body)=>{calls.push({path,body});if(body===undefined&&path.includes('/plans?'))return {planId:'plan-1',revisions:[{version:2,hash:'a'.repeat(64),content:'# 计划',createdAt:'2026-11-24T10:00:00Z'}]};return {};},
  };
  const replyStart=source.indexOf('async function respondToRequest('),replyEnd=source.indexOf('async function retryLocalSubmission(',replyStart);
  const planReview=readFileSync(require.resolve('../web/plan-review.js'),'utf8');
  vm.runInNewContext(planReview+'\n'+source.slice(elStart,elEnd)+'\n'+source.slice(replyStart,replyEnd)+'\n'+source.slice(start,end)+'\nthis.renderAcpApproval=renderAcpApproval;',context);
  return {calls,render:context.renderAcpApproval};
}

function loadCodexPlanApproval(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8'),calls=[];
  const start=source.indexOf('function renderPlanApproval'),end=source.indexOf('async function ignoreSubmission',start);
  const elStart=source.indexOf('function el('),elEnd=source.indexOf('\n',elStart);
  const context={
    window:{BridgePendingCommands:new PendingCommands(new MemoryStorage())},uuid:()=> 'command-1',chatKey:(id,host)=>host+'|'+id,
    currentId:'session',currentHost:'local',state:{agent:'kimi'},submittedRequestIds:new Set(),approvalBusy:0,
    document:{createElement:tag=>new FakeNode(tag)},t:value=>value,
    setLanguageBusy(){},sessionUrl:()=>'/respond',bumpRecency(){},renderMarkdown:(node,text)=>{node.renderedMarkdown=text;},
    workModes:{selection:'plan',render(){}},sessionStorage:{setItem(){}},
    api:async(path,body)=>{calls.push({path,body});return {};},
  };
  const replyStart=source.indexOf('async function respondToRequest('),replyEnd=source.indexOf('async function retryLocalSubmission(',replyStart);
  const planReview=readFileSync(require.resolve('../web/plan-review.js'),'utf8');
  vm.runInNewContext(planReview+'\n'+source.slice(elStart,elEnd)+'\n'+source.slice(replyStart,replyEnd)+'\n'+source.slice(start,end)+'\nthis.renderPlanApproval=renderPlanApproval;',context);
  return {calls,render:context.renderPlanApproval};
}

const request=()=>({id:'request-1',params:{options:[
  {id:'plan_approve',label:'Approve',kind:'allow_once'},
  {id:'plan_revise',label:'Revise',kind:'reject_once'},
]}});

test('ACP plan revision requires feedback and submits its text',async()=>{
  const {calls,render}=loadApproval(),card=render(request());
  const input=card.querySelectorAll('textarea')[0],revise=card.querySelectorAll('button').find(button=>button.textContent==='Revise');
  assert.ok(input);
  revise.onclick();
  assert.equal(calls.length,0);
  assert.equal(input.focused,true);
  assert.equal(card.children.at(-1).textContent,'请填写修改意见');
  input.value='补充错误处理';
  revise.onclick();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(calls.length,1);
  assert.equal(calls[0].body.response.optionId,'plan_revise');
  assert.equal(calls[0].body.response.text,'补充错误处理');
});

test('ACP permission heading is not repeated from params.title',()=>{
  const {render}=loadApproval();
  const card=render({id:'request-1',params:{title:'权限请求',command:'ls',options:[]}});
  const headings=card.children.filter(node=>node.textContent==='权限请求');
  assert.equal(headings.length,1);
  assert.equal(headings[0].tag,'h3');
  assert.equal(card.children[1].textContent,'ls');
});

test('ACP permission text uses the sanitized Markdown renderer',()=>{
  const {render}=loadApproval(),source='# 修复设计缺陷\n\n- 保持上下文';
  const card=render({id:'request-1',params:{text:source,options:[]}});
  const body=card.children.find(node=>node.className==='approval-markdown');
  assert.ok(body);
  assert.equal(body.renderedMarkdown,source);
});

test('ACP structured plan renders as Markdown without a duplicate command block',()=>{
  const {render}=loadApproval(),plan='# 修复设计缺陷\n\n- 保持上下文';
  const card=render({id:'request-1',params:{text:plan,planContent:plan,allowedPrompts:[{prompt:'run server tests',tool:'Bash'}],command:'should not appear',options:[]}});
  const body=card.children.find(node=>node.className==='approval-markdown');
  const operations=card.children.find(node=>node.className==='approval-plan-operations');
  assert.equal(body.renderedMarkdown,plan);
  assert.equal(card.children.some(node=>node.className==='approval-command'),false);
  assert.equal(operations.children[1].children[0].textContent,'run server tests (Bash)');
});

test('ACP revision feedback includes the displayed plan version',async()=>{
  const {calls,render}=loadApproval(),value=request();
  value.params.planContent='# 计划';
  value.params.planId='plan-1';value.params.planVersion=2;value.params.planHash='a'.repeat(64);
  const card=render(value),input=card.querySelectorAll('textarea')[0],revise=card.querySelectorAll('button').find(button=>button.textContent==='Revise');
  input.value='> 第一步\n\n补充错误处理';revise.onclick();
  await new Promise(resolve=>setImmediate(resolve));
  const text=calls[0].body.response.text;
  assert.ok(text.includes('Plan-Version: 2'));
  assert.ok(text.includes('Plan-SHA256: '+'a'.repeat(64)));
  assert.ok(text.endsWith('> 第一步\n\n补充错误处理'));
});

test('ACP plan history loads through the current session route',async()=>{
  const {calls,render}=loadApproval(),value=request();
  Object.assign(value.params,{planContent:'# 计划',planId:'plan-1',planVersion:2,planHash:'a'.repeat(64)});
  const card=render(value),history=card.children.find(node=>node.className==='plan-history');
  assert.ok(history);assert.equal(calls.length,0);
  history.open=true;await history.ontoggle();
  assert.equal(calls.length,1);
  assert.equal(calls[0].path,'/api/sessions/session/plans?host=local&planId=plan-1&agent=kimi');
  assert.equal(history.children[2].children[0].children[0].textContent.startsWith('版本 2'),true);
});

test('Codex plan revision uses the same version-bound feedback',async()=>{
  const {calls,render}=loadCodexPlanApproval();
  const card=render({id:'request-1',params:{planContent:'# 计划',planId:'turn-1',planVersion:3,planHash:'b'.repeat(64)}});
  const input=card.querySelectorAll('textarea')[0],revise=card.querySelectorAll('button').find(button=>button.textContent==='按照修改意见继续规划');
  input.value='补充验收标准';revise.onclick();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(calls[0].body.response.action,'revise');
  assert.ok(calls[0].body.response.text.includes('Plan-Version: 3'));
  assert.ok(calls[0].body.response.text.endsWith('补充验收标准'));
});

test('other ACP plan options keep their original response shape',async()=>{
  const {calls,render}=loadApproval(),card=render(request());
  const approve=card.querySelectorAll('button').find(button=>button.textContent==='Approve');
  approve.onclick();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(calls[0].body.response.optionId,'plan_approve');
  assert.equal('text' in calls[0].body.response,false);
});
