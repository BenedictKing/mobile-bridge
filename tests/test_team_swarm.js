'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {TeamSwarmPanel,teamSwarmCommand,teamSwarmBuildContract}=require('../web/team-swarm.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.style={};this.dataset={};this.className='';this.textContent='';this.listeners={};}
  append(...nodes){this.children.push(...nodes);}
  prepend(...nodes){this.children.unshift(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(name,listener){this.listeners[name]=listener;}
  click(){return this.listeners.click?.({preventDefault(){}});}
  input(value){this.value=value;this.listeners.input?.({target:this});}
  change(value){if(typeof value==='boolean')this.checked=value;else this.value=value;this.listeners.change?.({target:this});}
}
global.document={createElement:tag=>new FakeNode(tag)};
let language='zh';
global.BridgeI18n={language:()=>language,t:text=>text};
const text=node=>node.textContent+node.children.map(text).join('');
function all(root,predicate){return [root,...root.children.flatMap(child=>all(child,()=>true))].filter(predicate);}
const buttons=root=>all(root,node=>node.tag==='button');
const button=(root,label)=>buttons(root).find(node=>node.textContent===label);
const field=(root,name)=>all(root,node=>node.name===name)[0];
const flush=()=>new Promise(resolve=>setImmediate(resolve));
function run(overrides={}){return {id:'run-1',teamId:'team-1',version:7,goal:'重启后不丢任务，并改善手机体验',status:'waiting_input',tasks:[],swarm:{schemaVersion:1},...overrides};}
function draft(overrides={}){return {human:'',machine:'',command:'',paths:'.',workspaceId:'',taskId:'*',maxRepairs:'2',maxEvals:'12',timeoutSeconds:'600',requireIndependentReview:true,...overrides};}
function contract(overrides={}){return teamSwarmBuildContract(run(),draft(overrides));}
function setup(value=run(),request=async()=>({}),extra={}){const panel=new TeamSwarmPanel({run:value,request,...extra});return {panel,root:panel.render()};}

test('contract preserves the complete original goal and separates human from machine conditions',()=>{
  const value=contract({human:'手机操作可达',machine:'崩溃恢复后输入仍可读取\n相同 submission 不重复发送',command:'go\ntest\n./internal/team'});
  assert.deepEqual(value.criteria[0],{id:'original',text:run().goal,checkKind:'human',required:true});
  assert.deepEqual(value.evalSpecs[0].criterionIds,['check-1','check-2']);
  assert.deepEqual(value.evalSpecs[0].command,['go','test','./internal/team']);
  assert.deepEqual(value.tasks,[{taskId:'*',criterionIds:['original','human-1','check-1','check-2'],allowedWritePaths:['.'],evalSpecIds:['acceptance']}]);
  assert.equal(value.requireIndependentReview,true);
  assert.equal(value.maxRepairs,2);
});

test('argv input preserves argument spaces and rejects shell constructs and oversized commands',()=>{
  assert.deepEqual(teamSwarmCommand('go\ntest\n-run\nTest Name\n'),['go','test','-run','Test Name']);
  for(const command of ['go\ntest; echo secrets','go\ntest\n$(pwd)','go\ntest\n`pwd`','go\ntest\n|cat'])assert.throws(()=>teamSwarmCommand(command),/shell/);
  assert.throws(()=>teamSwarmCommand(Array(9).fill('x').join('\n')),/8/);
  assert.throws(()=>teamSwarmCommand('中'.repeat(172)),/512/);
  assert.throws(()=>contract({machine:'正确'}),/同时填写/);
  assert.throws(()=>contract({command:'go\ntest'}),/同时填写/);
  assert.throws(()=>contract({paths:' '}),/不能为空/);
  assert.throws(()=>contract({maxRepairs:'3'}),/有效/);
  assert.throws(()=>contract({maxEvals:'0'}),/有效/);
  assert.throws(()=>contract({maxEvals:'65'}),/有效/);
  assert.throws(()=>contract({human:Array(20).fill('条件').join('\n')}),/20/);
});

test('plain form submits a valid proposal and retains original input without protocol JSON',async()=>{
  const requests=[];
  const {panel,root}=setup(run(),async(url,options)=>{requests.push({url,...options});return {swarm:{schemaVersion:1,proposal:{id:'proposal-1',contract:options.body.contract}},version:8};});
  field(root,'machine').input('恢复输入');field(root,'command').input('go\ntest\n./internal/team');
  await button(root,'提出验收契约').click();
  assert.equal(requests.length,1);
  assert.equal(requests[0].url,'/api/teams/team-1/runs/run-1/contract-proposals');
  assert.equal(requests[0].method,'POST');
  assert.match(requests[0].body.id,/^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/);
  assert.equal(requests[0].body.version,7);assert.equal(requests[0].body.schemaVersion,1);
  assert.equal(requests[0].body.contract.criteria[0].text,run().goal);
  assert.equal(panel.run.version,8);
  assert.ok(button(root,'接受并冻结契约'));
});

test('incomplete machine check is rejected inline without sending',async()=>{
  let calls=0;const {root}=setup(run(),async()=>{calls++;});
  field(root,'machine').input('必须通过');await button(root,'提出验收契约').click();
  assert.equal(calls,0);assert.match(text(root),/请同时填写/);
});

test('pending writes disable all controls and duplicate clicks share one request',async()=>{
  let resolve;let calls=0;
  const {panel,root}=setup(run(),()=>{calls++;return new Promise(done=>{resolve=done;});});
  const submit=button(root,'提出验收契约'),pending=submit.click();
  assert.equal(submit.disabled,true);assert.equal(root.attributes['aria-busy'],'true');
  await submit.click();assert.equal(calls,1);
  const next=new TeamSwarmPanel({run:run(),request:panel.request,drafts:panel.drafts});
  assert.equal(button(next.render(),'提出验收契约').disabled,true);
  resolve({});await pending;
  assert.equal(button(root,'提出验收契约').disabled,false);
});

test('network retries reuse operation UUID and conflicts stay visible',async()=>{
  const bodies=[];
  const {root}=setup(run(),async(url,{body})=>{bodies.push(body);const error=new Error('stale version');error.status=bodies.length===2?409:500;throw error;});
  await button(root,'提出验收契约').click();await button(root,'提出验收契约').click();
  assert.equal(bodies[0].id,bodies[1].id);
  assert.match(text(root),/版本已变化/);
  assert.equal(button(root,'提出验收契约').disabled,false);
});

test('a successful write followed by refresh failure is reported as recorded',async()=>{
  let calls=0;
  const {root}=setup(run(),async()=>{calls++;return {version:8,swarm:{schemaVersion:1,proposal:{id:'proposal',contract:contract()}}};},{onChange:async()=>{throw new Error('offline');}});
  await button(root,'提出验收契约').click();
  assert.equal(calls,1);assert.match(text(root),/操作已记录，但刷新失败/);
  assert.ok(button(root,'接受并冻结契约'));
});

test('acceptance uses the exact proposal identity, current version and schema',async()=>{
  const calls=[];
  const value=run({swarm:{schemaVersion:1,proposal:{id:'proposal/a',contract:contract()}}});
  const {root}=setup(value,async(url,options)=>{calls.push({url,...options});return {};});
  await button(root,'接受并冻结契约').click();
  assert.equal(calls[0].url,'/api/teams/team-1/runs/run-1/contract-proposals/proposal%2Fa/accept');
  assert.equal(calls[0].body.version,7);assert.equal(calls[0].body.schemaVersion,1);
});

test('stale and missing validity remain visible even when an evaluation passed',()=>{
  const value=run({swarm:{schemaVersion:1,contract:contract(),evals:[{id:'e1',status:'passed',validity:'stale',outputHead:'<script>unsafe()</script>',exitCode:0},{id:'e2',status:'passed'}]}});
  const {root}=setup(value);
  assert.match(text(root),/已过期/);assert.match(text(root),/有效性未知/);
  assert.match(text(root),/不能用于当前验收/);assert.doesNotMatch(text(root),/目标已达成/);
  const pre=all(root,node=>node.tag==='pre')[0];assert.equal(pre.textContent,'<script>unsafe()</script>');
  assert.equal(all(root,node=>node.tag==='script').length,0);
});

test('local snapshot isolation comes from the actual artifact when eval records omit it',()=>{
  const value=run({swarm:{schemaVersion:1,contract:contract(),artifacts:[{id:'a1',kind:'task',taskId:'impl',contentHash:'hash',isolation:'local_snapshot'}],evals:[{id:'e1',artifactId:'a1',status:'passed',validity:'fresh',exitCode:0}]}});
  const {root}=setup(value);
  const evalRow=all(root,node=>node.dataset?.evalId==='e1')[0];
  assert.match(text(evalRow),/不等同于操作系统沙箱/);
  assert.match(text(all(root,node=>node.dataset?.artifactId==='a1')[0]),/不等同于操作系统沙箱/);
});

test('evaluation controls select only completed execution tasks and use declared spec',async()=>{
  const calls=[];
  const value=run({tasks:[{id:'running',status:'running'},{id:'review',status:'completed',review:true},{id:'impl',status:'completed'}],swarm:{schemaVersion:1,contract:contract({machine:'恢复输入',command:'go\ntest'}),evals:[]}});
  const {root}=setup(value,async(url,options)=>{calls.push({url,...options});return {};});
  assert.deepEqual(field(root,'evalSubject').children.map(x=>x.value),['task:impl']);
  await button(root,'运行候选检查').click();
  assert.equal(calls[0].url,'/api/teams/team-1/runs/run-1/swarm/evaluate');
  assert.equal(calls[0].body.taskId,'impl');assert.equal(calls[0].body.evalSpecId,'acceptance');
});

test('integrated artifacts can be evaluated directly without pretending to be a task',async()=>{
  const calls=[];
  const value=run({swarm:{schemaVersion:1,contract:contract({machine:'组合结果满足约定',command:'go\ntest'}),artifacts:[{id:'integrated',kind:'integration',contentHash:'hash',review:{verdict:'passed',independent:true,validity:'fresh'}}]}});
  const {root}=setup(value,async(url,options)=>{calls.push({url,...options});return {};});
  assert.deepEqual(field(root,'evalSubject').children.map(x=>x.value),['artifact:integrated']);
  await button(root,'运行候选检查').click();
  assert.equal(calls[0].body.artifactId,'integrated');assert.equal(calls[0].body.taskId,undefined);
  assert.match(text(root),/独立审查 · 已通过 · 有效/);assert.doesNotMatch(text(root),/未取得独立审查结论/);
});

test('integration requires two selected actual candidates and keeps integration output separate',async()=>{
  const calls=[];
  const value=run({swarm:{schemaVersion:1,contract:contract(),artifacts:[{id:'a',taskId:'impl-a',contentHash:'abc'},{id:'b',taskId:'impl-b',contentHash:'def'},{id:'i',kind:'integration',contentHash:'ghi'}]}});
  const {root}=setup(value,async(url,options)=>{calls.push({url,...options});return {};});
  const action=button(root,'集成所选候选');assert.equal(action.disabled,true);
  const choices=all(root,node=>node.type==='checkbox');assert.equal(choices.length,2);
  choices[0].change(true);assert.equal(action.disabled,true);
  choices[1].change(true);assert.equal(action.disabled,false);await action.click();
  assert.equal(calls[0].url,'/api/teams/team-1/runs/run-1/swarm/integrate');
  assert.deepEqual(calls[0].body.artifactIds,['a','b']);assert.match(text(root),/集成候选/);
});

test('paused runs and active evaluation attempts cannot start new checks',()=>{
  const value=run({status:'paused',tasks:[{id:'impl',status:'completed'}],swarm:{schemaVersion:1,contract:contract({machine:'恢复输入',command:'go\ntest'}),evals:[{id:'e1',status:'running',validity:'unknown'}]}});
  const {root}=setup(value);assert.equal(button(root,'运行候选检查').disabled,true);assert.equal(button(root,'复验此检查').disabled,true);
  assert.equal(button(root,'刷新证据').disabled,false);
});

test('unknown schema is read-only and English fallback translates interface without translating user content',()=>{
  language='en';
  try{
    const {root}=setup(run({swarm:{schemaVersion:3}}));
    assert.match(text(root),/Update the client/);assert.equal(buttons(root).length,1);
    const {root:current}=setup();assert.match(text(current),/Original requirement/);assert.match(text(current),/重启后不丢任务，并改善手机体验/);
  }finally{language='zh';}
});

test('drafts survive periodic render and stay isolated between runs',async()=>{
  const drafts=new Map();const {root}=setup(run(),async()=>({}),{drafts});
  field(root,'human').input('我的体验要求');
  const {root:rerender}=setup(run({version:8}),async()=>({}),{drafts});
  assert.equal(field(rerender,'human').value,'我的体验要求');
  const {root:other}=setup(run({id:'run-2'}),async()=>({}),{drafts});
  assert.equal(field(other,'human').value,'');
  await flush();
});

test('refreshing Swarm updates the authoritative version used by the next mutation',async()=>{
  const requests=[];
  const {root}=setup(run(),async(url,options)=>{
    requests.push({url,options});
    if(!options)return {schemaVersion:1,runVersion:11,status:'awaiting_contract'};
    return {};
  });
  await button(root,'刷新证据').click();
  await button(root,'提出验收契约').click();
  assert.equal(requests[0].url,'/api/teams/team-1/runs/run-1/swarm');
  assert.equal(requests[1].options.body.version,11);
});

test('budget display uses cumulative server usage instead of local evaluation count',()=>{
  const {root}=setup(run({swarm:{schemaVersion:1,contract:contract(),evals:[],budget:{repairsUsed:2,maxRepairs:2,evalsUsed:9,maxEvals:12}}}));
  assert.match(text(root),/修复次数: 2\/2 · 评测次数: 9\/12/);
});

function diffPanel(request,artifact={id:'candidate/1',taskId:'impl',contentHash:'tree-1'}){
  return setup(run({swarm:{schemaVersion:1,contract:contract(),artifacts:[artifact]}}),request);
}
function openDiff(root){const details=all(root,node=>node.dataset?.artifactDiffId)[0];details.open=true;details.listeners.toggle();return details;}

test('candidate diff is loaded only on demand, identity checked and rendered as literal text',async()=>{
  const requests=[];
  const {root}=diffPanel(async url=>{requests.push(url);return {artifactId:'candidate/1',diff:'diff --git a/a b/a\n+<script>unsafe()</script>\n',truncated:false};});
  assert.equal(requests.length,0);openDiff(root);await flush();
  assert.deepEqual(requests,['/api/teams/team-1/runs/run-1/artifacts/candidate%2F1/diff']);
  const details=all(root,node=>node.dataset?.artifactDiffId)[0];assert.equal(details.open,true);
  assert.match(text(details),/\+<script>unsafe\(\)<\/script>/);
  assert.equal(all(details,node=>node.tag==='script').length,0);
  assert.equal(buttons(details).some(node=>/应用|Apply/.test(node.textContent)),false);
  details.open=false;details.listeners.toggle();details.open=true;details.listeners.toggle();await flush();
  assert.equal(requests.length,1,'reopening reuses the exact artifact content cache');
});

test('pending diff loads do not duplicate and truncated diffs explicitly cannot be completely applied',async()=>{
  let resolve,calls=0;
  const {root}=diffPanel(()=>{calls++;return new Promise(done=>{resolve=done;});});
  openDiff(root);assert.match(text(root),/正在读取差异/);
  openDiff(root);assert.equal(calls,1);
  resolve({artifactId:'candidate/1',diff:'partial diff',truncated:true});await flush();
  assert.match(text(root),/差异已截断，仅供查看，不能据此完整应用/);
});

test('diff errors remain local and retry requires an explicit action',async()=>{
  let calls=0;
  const {root}=diffPanel(async()=>{
    calls++;
    if(calls===1)throw new Error('artifact unavailable');
    return {artifactId:'candidate/1',diff:'recovered diff',truncated:false};
  });
  openDiff(root);await flush();assert.match(text(root),/artifact unavailable/);
  openDiff(root);await flush();assert.equal(calls,1,'rerenders and reopening must not trigger a retry loop');
  await button(root,'重试读取差异').click();assert.equal(calls,2);assert.match(text(root),/recovered diff/);
});

test('a diff response for a different artifact is rejected and changed content is not reused',async()=>{
  const drafts=new Map(),requests=[];
  const artifact={id:'candidate',contentHash:'v1'};
  const value=run({swarm:{schemaVersion:1,contract:contract(),artifacts:[artifact]}});
  const request=async url=>{requests.push(url);return {artifactId:'other',diff:'must not display',truncated:false};};
  const {root}=setup(value,request,{drafts});openDiff(root);await flush();
  assert.match(text(root),/读取差异失败/);assert.doesNotMatch(text(root),/must not display/);
  const {root:changed}=setup({...value,swarm:{...value.swarm,artifacts:[{...artifact,contentHash:'v2'}]}},request,{drafts});
  openDiff(changed);await flush();assert.equal(requests.length,2);
});
