'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class Card{
  constructor(id){this.dataset={request:id};this.value='';this.removed=false;this.destroyed=0;this.parent=null;}
  remove(){this.removed=true;if(this.parent){const index=this.parent.children.indexOf(this);if(index>=0)this.parent.children.splice(index,1);}}
  planReviewDestroy(){this.destroyed++;}
}
function setup(){
  const root={children:[],insertBefore(card,before){card.parent=this;if(this.children.includes(card))this.children.splice(this.children.indexOf(card),1);const index=before?this.children.indexOf(before):-1;if(index<0)this.children.push(card);else this.children.splice(index,0,card);}};
  let language='zh';
  const context={approvalStamp:'',BridgeI18n:{language:()=>language},$:()=>root,orderedApprovals:requests=>requests,renderAcpInput:request=>new Card(request.id)};
  const source=readFileSync(require.resolve('../web/app.js'),'utf8'),start=source.indexOf('function renderApprovals('),end=source.indexOf('function renderComputerApproval',start);
  vm.runInNewContext(source.slice(start,end)+'\nthis.render=renderApprovals;',context);
  return {root,render:context.render,language:value=>{language=value;}};
}
const request=(id,params={})=>({id,method:'acp/requestInput',supported:true,params});

test('adding and reordering requests keeps other cards and their input',()=>{
  const {root,render}=setup(),first=request('one'),second=request('two');
  render([first]);const card=root.children[0];card.value='正在填写';
  render([first,second]);assert.equal(root.children[0],card);assert.equal(card.value,'正在填写');
  const other=root.children[1];render([second,first]);
  assert.deepEqual(root.children,[other,card]);assert.equal(card.value,'正在填写');
});

test('changed content replaces only its card and runs cleanup hooks',()=>{
  const {root,render}=setup(),first=request('one'),second=request('two');
  render([first,second]);const old=root.children[0],kept=root.children[1];
  render([request('one',{question:'changed'}),second]);
  assert.notEqual(root.children[0],old);assert.equal(root.children[1],kept);
  assert.equal(old.removed,true);assert.equal(old.destroyed,1);assert.equal(kept.destroyed,0);
});

test('language changes rebuild cards and removing all requests destroys them',()=>{
  const {root,render,language}=setup(),first=request('one');
  render([first]);const old=root.children[0];language('en');render([first]);
  assert.notEqual(root.children[0],old);assert.equal(old.destroyed,1);
  const current=root.children[0];render([]);
  assert.equal(root.children.length,0);assert.equal(current.destroyed,1);
});
