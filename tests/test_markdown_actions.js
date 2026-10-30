'use strict';
// markdown.js 用真实 markdown-it 渲染 Lead 动作信封：合法块成卡片、代码里的协议示例保持原文，
// 复制按钮的代码块序号与渲染同一切片对齐，没有信封的消息渲染结果不变。
const test=require('node:test');
const assert=require('node:assert/strict');
const path=require('node:path');
const web=path.join(__dirname,'..','web');

class Node{
  constructor(tag){this.tag=tag;this.children=[];this.className='';this.textContent='';this.dataset={};this.attributes={};}
  append(...nodes){for(const node of nodes)this.children.push(typeof node==='string'?Object.assign(new Node('#text'),{textContent:node}):node);}
  setAttribute(name,value){this.attributes[name]=value;}
}
global.document={createElement:tag=>new Node(tag),createTextNode:text=>Object.assign(new Node('#text'),{textContent:text})};
const copies=[];
global.window={markdownit:require(path.join(web,'vendor','markdown-it.min.js')),katex:{render(){}},BridgeClipboard:{copy:producer=>copies.push(producer)}};
global.texmath=require(path.join(web,'vendor','texmath.js'));
global.BridgeConnection={url:value=>value};
const chat=require(path.join(web,'team-chat.js'));
global.splitTeamActions=chat.splitTeamActions;global.renderTeamActionsCard=chat.renderTeamActionsCard;
require(path.join(web,'markdown.js'));

function find(node,cls,out=[]){if(String(node.className).split(/\s+/).includes(cls))out.push(node);node.children.forEach(child=>find(child,cls,out));return out;}
function text(node){return node.textContent+node.children.map(text).join('');}
function render(source){
  copies.length=0;
  const node=new Node('div');
  window.renderMarkdown(node,source,[],()=>'',{fullText:async()=>source});
  return node;
}
const envelope='<team-actions>\n{"actions":[{"type":"delegate","member":"m1","task":"go"}]}\n</team-actions>';

test('code copy buttons stay aligned after an envelope becomes a card',async()=>{
  const source='计划\n\n'+envelope+'\n\n```js\nconsole.log(1)\n```\n\n```sh\necho 2\n```\n';
  const node=render(source);
  assert.equal(find(node,'team-actions-card').length,1);
  find(node,'code-copy').forEach(button=>button.onclick());
  assert.deepEqual(await Promise.all(copies.map(producer=>producer())),['console.log(1)\n','echo 2\n']);
});

test('protocol examples inside code stay as code',async()=>{
  const indented='示例：\n\n    '+envelope.replace(/\n/g,'\n    ')+'\n\n```js\nconsole.log(1)\n```\n';
  const node=render(indented);
  assert.equal(find(node,'team-actions-card').length,0);
  find(node,'code-copy').forEach(button=>button.onclick());
  const copied=await Promise.all(copies.map(producer=>producer()));
  assert.equal(copied.length,2,'缩进信封与围栏各算一个代码块');
  assert.equal(copied[1],'console.log(1)\n','复制按钮仍取到对应的代码块');
  const quoted=render('> ```\n> '+envelope.replace(/\n/g,'\n> ')+'\n> ```\n');
  assert.equal(find(quoted,'team-actions-card').length,0);
  assert.match(text(quoted),/<team-actions>/);
});

test('messages without envelopes render as before',()=>{
  const node=render('# 标题\n\n- 一\n- 二\n\n`code` 与 <team-actions> 字样');
  assert.equal(find(node,'team-actions-card').length,0);
  assert.match(text(node),/<team-actions> 字样/);
});
