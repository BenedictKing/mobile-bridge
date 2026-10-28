'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode {
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.disabled=false;this.value='';}
  append(...nodes){this.children.push(...nodes);}
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
    currentId:'session',currentHost:'local',submittedRequestIds:new Set(),approvalBusy:0,
    document:{createElement:tag=>new FakeNode(tag)},t:value=>value,
    setLanguageBusy(){},sessionUrl:()=>'/respond',bumpRecency(){},riskBadge:()=>null,markRecommendation(){},
    api:async(path,body)=>{calls.push({path,body});return {};},
  };
  vm.runInNewContext(source.slice(elStart,elEnd)+'\n'+source.slice(start,end)+'\nthis.renderAcpApproval=renderAcpApproval;',context);
  return {calls,render:context.renderAcpApproval};
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

test('other ACP plan options keep their original response shape',async()=>{
  const {calls,render}=loadApproval(),card=render(request());
  const approve=card.querySelectorAll('button').find(button=>button.textContent==='Approve');
  approve.onclick();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(calls[0].body.response.optionId,'plan_approve');
  assert.equal('text' in calls[0].body.response,false);
});
