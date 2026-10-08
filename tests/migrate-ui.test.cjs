'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const VIEW={agent:'codex',agents:[{kind:'codex',label:'Codex'},{kind:'kimi',label:'Kimi'},{kind:'mcode',label:'MiniMax'}]};
function fixture({confirmResult=true,requestError=null}={}){
  function node(tag){return {tag,value:'',textContent:'',disabled:false,hidden:false,children:[],dataset:{},type:'',className:'',
    append(...v){this.children.push(...v);},replaceChildren(...v){this.children=v;},
    setAttribute(k,v){this[k]=v;},
    querySelectorAll(selector){const found=[];const walk=x=>{for(const c of x.children||[]){if(selector==='button'&&c.tag==='button')found.push(c);walk(c);}};walk(this);return found;}};}
  const calls={requests:[],migrated:[],errors:[],confirms:[]};
  const ctx=vm.createContext({crypto:require('node:crypto').webcrypto,document:{createElement:node},window:{},root:node('div'),calls,
    confirmResult,requestError});
  for(const name of ['i18n.js','migrate.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,'../web',name),'utf8'),ctx);
  vm.runInContext(`control=new MigrateControl({root,
    request:async body=>{calls.requests.push(body);if(requestError)throw Error(requestError);return {id:'new-1',host:'local',agent:body.agent,status:'sent',sent:true};},
    confirm:message=>{calls.confirms.push(message);return confirmResult;},
    onMigrated:(result,label)=>calls.migrated.push([result,label]),
    onError:error=>calls.errors.push(error)});`,ctx);
  return {control:ctx.control,root:ctx.root,calls,ctx,
    buttons:()=>ctx.root.querySelectorAll('button'),
    text:()=>{const out=[];const walk=x=>{if(x.textContent)out.push(x.textContent);(x.children||[]).forEach(walk);};walk(ctx.root);return out.join('\n');},
    language:()=>vm.runInContext("BridgeI18n.setLanguage('en')",ctx)};
}
test('lists every enabled agent except the current one and hides otherwise',()=>{
  const ui=fixture();
  ui.control.render(VIEW);
  assert.equal(ui.root.hidden,false);
  assert.deepEqual(ui.buttons().map(b=>b.dataset.agent),['kimi','mcode']);
  assert.match(ui.text(),/迁移到其他 Agent/);
  assert.match(ui.text(),/迁移到 Kimi/);
  ui.control.render({agent:'kimi',agents:VIEW.agents});
  assert.deepEqual(ui.buttons().map(b=>b.dataset.agent),['codex','mcode']);
  ui.control.render({agent:'codex',agents:[{kind:'codex',label:'Codex'}]});
  assert.equal(ui.root.hidden,true);
  assert.equal(ui.buttons().length,0);
  ui.control.render({agent:'codex'});
  assert.equal(ui.root.hidden,true);
});
test('confirm gates the request and success reports back with the label',async()=>{
  const ui=fixture({confirmResult:false});
  ui.control.render(VIEW);
  ui.buttons()[0].onclick();
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ui.calls.requests.length,0);
  assert.match(ui.calls.confirms[0],/源会话保持不变。/);
  const ok=fixture();
  ok.control.render(VIEW);
  ok.buttons()[0].onclick();
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ok.calls.requests.length,1);
  assert.equal(ok.calls.requests[0].agent,'kimi');
  assert.match(ok.calls.requests[0].id,/^[0-9a-f-]{36}$/);
  assert.equal(ok.calls.requests[0].uiLocale,'zh-CN');
  assert.equal(ok.calls.migrated[0][0].id,'new-1');
  assert.equal(ok.calls.migrated[0][1],'Kimi');
});
test('failure surfaces the error and re-enables the buttons',async()=>{
  const ui=fixture({requestError:'目标 Agent 没有与源会话相同目录的项目，请先在目标 Agent 添加该项目'});
  ui.control.render(VIEW);
  ui.buttons()[0].onclick();
  assert.equal(ui.buttons()[0].disabled,true);
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ui.calls.errors.length,1);
  assert.match(ui.calls.errors[0].message,/相同目录/);
  assert.equal(ui.buttons()[0].disabled,false);
});
test('english locale translates labels and confirm text',async()=>{
  const ui=fixture();
  ui.language();
  ui.control.render(VIEW);
  assert.match(ui.text(),/Migrate to another agent/);
  assert.match(ui.text(),/Migrate to Kimi/);
  ui.buttons()[1].onclick();
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ui.calls.requests[0].agent,'mcode');
  assert.equal(ui.calls.requests[0].uiLocale,'en-US');
  assert.match(ui.calls.confirms[0],/source session stays unchanged/);
});
