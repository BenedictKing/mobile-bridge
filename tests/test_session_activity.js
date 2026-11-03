'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');

class FakeNode{
  constructor(tag){this.tag=tag;this.className='';this.children=[];this.dataset={};this.title='';this.textContent='';}
  append(...nodes){this.children.push(...nodes);}
  prepend(...nodes){this.children.unshift(...nodes);}
  setAttribute(){}
  remove(){this.removed=true;}
  // 支持 .class 与标签两种选择器；文本子节点跳过。
  querySelector(selector){
    const match=node=>typeof node!=='string'&&(selector.startsWith('.')?
      node.className.split(/\s+/).includes(selector.slice(1)):node.tag===selector);
    return this.children.find(match)||null;
  }
}

// 模拟浏览器的 querySelectorAll：'.session[data-id]' 只命中带 data-id 的会话行，
// 回归成 '.session' 时项目团队入口也会被返回，测试即失败。
function queryAll(nodes,selector){
  const [base,attr]=selector.split('[');
  const attribute=attr?attr.replace(']',''):null;
  // data-id 在 DOM 里映射为 dataset.id（连字符转驼峰）。
  const key=attribute?attribute.replace(/^data-/,'').replace(/-(\w)/g,(_,letter)=>letter.toUpperCase()):null;
  return nodes.filter(node=>node.className.split(/\s+/).includes(base.slice(1))&&
    (!key||node.dataset[key]!==undefined));
}

function loadRender(){
  const source=readFileSync(require.resolve('../web/app.js'),'utf8');
  const elStart=source.indexOf('function el('),elEnd=source.indexOf('\n',elStart);
  const keyStart=source.indexOf('function chatKey('),keyEnd=source.indexOf('\n',keyStart);
  const start=source.indexOf('function renderActivity()'),end=source.indexOf('async function refreshActivity',start);
  const chat=row('session',{id:'chat-1',host:'local'},'最近聊天');
  const team=row('session default-team',{project:'codex|/work'},'项目团队');
  const context={
    sessionActivity:{indicator:key=>(key==='local|chat-1'?{kind:'running',label:'运行中'}:null)},
    document:{
      createElement:tag=>new FakeNode(tag),
      querySelectorAll:selector=>queryAll([chat.button,team.button],selector),
    },
    t:value=>value,
  };
  vm.runInNewContext(source.slice(elStart,elEnd)+'\n'+source.slice(keyStart,keyEnd)+'\n'+
    source.slice(start,end)+'\nthis.render=renderActivity;',context);
  context.render();
  return{chat,team};
}

function row(className,dataset,label){
  const button=new FakeNode('button');
  button.className=className;button.dataset=dataset;
  const strong=new FakeNode('strong');
  strong.textContent=label;button.append(strong);
  return{button,strong};
}

test('project team entry never inherits the open chat activity badge',()=>{
  const{chat,team}=loadRender();
  assert.deepEqual(chat.strong.children.map(node=>node.textContent||node.title),['运行中','运行中']);
  assert.equal(chat.strong.children[0].className,'session-activity-label');
  assert.equal(team.strong.children.length,0);
  assert.equal(team.strong.textContent,'项目团队');
});

test('terminal turn badges distinguish completion, failure and refusal interruption',()=>{
  const {SessionActivity}=require('../web/activity.js');
  const values=new Map();
  const storage={getItem:key=>values.get(key)||null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};
  const cases=[
    ['completed','completed','已完成，未查看'],
    ['failed','ended','运行失败，未查看'],
    ['interrupted','ended','已停止，未查看'],
  ];
  for(const [turnStatus,kind,label] of cases){
    const activity=new SessionActivity({storage});
    const row={host:'local',id:'chat-'+turnStatus,connected:true,status:'idle',turnId:'turn-'+turnStatus,turnStatus,pendingRequests:0};
    activity.update([row]);
    const key=activity.id(row);
    assert.deepEqual(activity.indicator(key),{kind,label});
    activity.update([{...row,connected:false}]);
    assert.deepEqual(activity.indicator(key),{kind,label},turnStatus+' offline badge');
    activity.read(key);
    assert.equal(activity.indicator(key),null);
  }
});
