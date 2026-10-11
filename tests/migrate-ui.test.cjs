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
    agentButtons:()=>ctx.root.querySelectorAll('button').filter(b=>b.dataset.agent),
    scopeButtons:()=>ctx.root.querySelectorAll('button').filter(b=>b.dataset.scope),
    text:()=>{const out=[];const walk=x=>{if(x.textContent)out.push(x.textContent);(x.children||[]).forEach(walk);};walk(ctx.root);return out.join('\n');},
    language:()=>vm.runInContext("BridgeI18n.setLanguage('en')",ctx)};
}
test('lists every enabled agent except the current one and hides otherwise',()=>{
  const ui=fixture();
  ui.control.render(VIEW);
  assert.equal(ui.root.hidden,false);
  assert.deepEqual(ui.agentButtons().map(b=>b.dataset.agent),['kimi','mcode']);
  assert.match(ui.text(),/迁移到其他 Agent/);
  assert.match(ui.text(),/迁移到 Kimi/);
  ui.control.render({agent:'kimi',agents:VIEW.agents});
  assert.deepEqual(ui.agentButtons().map(b=>b.dataset.agent),['codex','mcode']);
  ui.control.render({agent:'codex',agents:[{kind:'codex',label:'Codex'}]});
  assert.equal(ui.root.hidden,true);
  assert.equal(ui.buttons().length,0);
  ui.control.render({agent:'codex'});
  assert.equal(ui.root.hidden,true);
});
test('picking a target reveals copy scopes and back returns to the agent list',()=>{
  const ui=fixture();
  ui.control.render(VIEW);
  ui.agentButtons()[0].onclick();  // 迁移到 Kimi
  assert.deepEqual(ui.scopeButtons().map(b=>b.dataset.scope),['full','main','latest']);
  assert.match(ui.text(),/选择复制范围/);
  assert.match(ui.text(),/完全复制历史/);
  assert.match(ui.text(),/只复制主要历史/);
  assert.match(ui.text(),/只复制最新提问和结果/);
  assert.match(ui.text(),/返回/);
  assert.equal(ui.agentButtons().length,0);
  // State refreshes keep the picker on the scope step for the same chat.
  ui.control.render(VIEW);
  assert.deepEqual(ui.scopeButtons().map(b=>b.dataset.scope),['full','main','latest']);
  // A different chat's details reset to the agent list.
  ui.control.render({...VIEW,id:'other-session'});
  assert.deepEqual(ui.agentButtons().map(b=>b.dataset.agent),['kimi','mcode']);
  ui.agentButtons()[0].onclick();
  ui.buttons().filter(b=>!b.dataset.agent&&!b.dataset.scope)[0].onclick();  // 返回
  assert.deepEqual(ui.agentButtons().map(b=>b.dataset.agent),['kimi','mcode']);
});
test('confirm gates the request and success reports back with the label',async()=>{
  const ui=fixture({confirmResult:false});
  ui.control.render(VIEW);
  ui.agentButtons()[0].onclick();
  ui.scopeButtons()[1].onclick();  // 只复制主要历史
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ui.calls.requests.length,0);
  assert.match(ui.calls.confirms[0],/源会话保持不变。/);
  assert.match(ui.calls.confirms[0],/最近消息转录/);
  const ok=fixture();
  ok.control.render(VIEW);
  ok.agentButtons()[0].onclick();
  ok.scopeButtons()[1].onclick();
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ok.calls.requests.length,1);
  assert.equal(ok.calls.requests[0].agent,'kimi');
  assert.equal(ok.calls.requests[0].scope,'main');
  assert.match(ok.calls.requests[0].id,/^[0-9a-f-]{36}$/);
  assert.equal(ok.calls.requests[0].uiLocale,'zh-CN');
  assert.equal(ok.calls.migrated[0][0].id,'new-1');
  assert.equal(ok.calls.migrated[0][1],'Kimi');
});
test('each scope reaches the request and the confirm text',async()=>{
  const ui=fixture();
  ui.control.render(VIEW);
  ui.agentButtons()[0].onclick();  // Enter the scope step once.
  for(const [index,scope,word] of [[0,'full','完整历史转录'],[2,'latest','最新提问与回答']]){
    ui.scopeButtons()[index].onclick();
    await new Promise(r=>setTimeout(r,10));
    assert.equal(ui.calls.requests.length,index>0?2:1);
    const request=ui.calls.requests[ui.calls.requests.length-1];
    assert.equal(request.agent,'kimi');
    assert.equal(request.scope,scope);
    const confirm=ui.calls.confirms[ui.calls.confirms.length-1];
    assert.match(confirm,new RegExp(word));
    assert.match(confirm,/源会话保持不变。/);
  }
});
test('failure surfaces the error and re-enables the buttons',async()=>{
  const ui=fixture({requestError:'目标 Agent 没有与源会话相同目录的项目，请先在目标 Agent 添加该项目'});
  ui.control.render(VIEW);
  ui.agentButtons()[0].onclick();
  ui.scopeButtons()[0].onclick();
  assert.equal(ui.buttons()[0].disabled,true);
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ui.calls.errors.length,1);
  assert.match(ui.calls.errors[0].message,/相同目录/);
  // Re-rendered controls are enabled again and stay on the scope step.
  assert.equal(ui.buttons().every(b=>!b.disabled),true);
  assert.deepEqual(ui.scopeButtons().map(b=>b.dataset.scope),['full','main','latest']);
});
test('english locale translates labels and confirm text',async()=>{
  const ui=fixture();
  ui.language();
  ui.control.render(VIEW);
  assert.match(ui.text(),/Migrate to another agent/);
  assert.match(ui.text(),/Migrate to Kimi/);
  ui.agentButtons()[1].onclick();
  assert.match(ui.text(),/Choose what to copy/);
  assert.match(ui.text(),/Copy full history/);
  ui.scopeButtons()[0].onclick();
  await new Promise(r=>setTimeout(r,10));
  assert.equal(ui.calls.requests[0].agent,'mcode');
  assert.equal(ui.calls.requests[0].scope,'full');
  assert.equal(ui.calls.requests[0].uiLocale,'en-US');
  assert.match(ui.calls.confirms[0],/complete history transcript/);
  assert.match(ui.calls.confirms[0],/source session stays unchanged/);
});
