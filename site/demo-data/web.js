'use strict';
// Sample-data fixtures for the offline website demo. Nothing here touches a
// real gateway: every request is answered locally and unknown calls fail closed.
(()=>{
const T=window.demoText;
const ids=['11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222','33333333-3333-4333-8333-333333333333'];
const teamId='website-team',runId='run-2417';
const now=Date.now();
const sessions=[
 {id:ids[0],host:'local',hostLabel:T('此电脑','This computer'),title:T('团队：首页改版排期','Team: homepage revision'),cwd:'/projects/website',projectName:'website',projectKey:'local|website',agent:'codex',recency:now-2*60000},
 {id:ids[1],host:'local',hostLabel:T('此电脑','This computer'),title:T('首页改版：导航与首屏','Homepage: navigation and hero'),cwd:'/projects/website',projectName:'website',projectKey:'local|website',agent:'claude',recency:now-38*60000},
 {id:ids[2],host:'local',hostLabel:T('此电脑','This computer'),title:T('账号池巡检记录','Account pool health check'),cwd:'/projects/ops',projectName:'ops',projectKey:'local|ops',agent:'kimi',recency:now-26*3600000}
];
const agents=[{kind:'claude',label:'Claude'},{kind:'kimi',label:'Kimi'},{kind:'zcode',label:'ZCode'},{kind:'workbuddy',label:'CodeBuddy'}];
const approval={id:'req-771',method:'item/commandExecution/requestApproval',supported:true,recommend:'accept',
 params:{command:'git push origin main',cwd:'/projects/website',reason:T('推送这次改版提交到远端仓库','Push this revision to the remote repository')},
 assessment:{source:'jev',riskLabel:'elevated',confidence:0.82,reversible:0.94,reason:'recommend_review',negativeSum:0.63,threshold:0.6}};
const modelFor=agent=>agent==='claude'?{model:'claude-sonnet-4-6',provider:'anthropic'}:agent==='kimi'?{model:'kimi-k2',provider:'moonshot'}:{model:'gpt-6.1-sol',provider:'openai'};
const baseMeta=(session,status='idle')=>({connected:true,status,agent:session.agent,...modelFor(session.agent),effort:'high',permissionMode:'ask',serviceTier:'default',collaborationMode:'default',historyComplete:true,readOnly:false,title:session.title,host:session.host,hostLabel:session.hostLabel,cwd:session.cwd,projectName:session.projectName,projectKey:session.projectKey,capabilities:['permissions','plan','goal','queue','skills'],agents,usage:{used:73400,size:200000,stale:false}});
const meta=new Map([
 [ids[0],{...baseMeta(sessions[0],'active'),requests:[],submissions:[]}],
 [ids[1],{...baseMeta(sessions[1]),requests:[approval],submissions:[{id:'sub-1',text:T('提交前把差异贴给我看一下。','Show me the diff before you commit.'),mode:'queue',status:'queued'}]}],
 [ids[2],{...baseMeta(sessions[2]),requests:[],submissions:[]}]
]);
let sequence=1;
const lastSequence=new Map();
const rows=new Map([
 [ids[0],[
  {key:'u1',role:'user',text:T('按评审意见把首页改版拆成任务，交给团队跑一轮。','Split the homepage revision into tasks and hand them to the team.')},
  {key:'a1',role:'assistant',text:T('拆成四步交给团队：\n\n- 梳理评审意见并拆任务（Codex，主控）\n- 合并导航层级、精简首屏（Kimi）\n- 评审改动与可访问性（Claude）\n- 按评审意见修订并补测试（Kimi）\n\n当前在等第三步的评审结论。','Handing four steps to the team:\n\n- Read the review notes and split the work (Codex, lead)\n- Flatten navigation and shorten the hero (Kimi)\n- Review the change and accessibility (Claude)\n- Apply the feedback and add tests (Kimi)\n\nWaiting on the review result now.')}
 ]],
 [ids[1],[
  {key:'u1',role:'user',text:T('帮我把首页的导航和首屏文案改一版，按上次的评审意见来。','Rework the homepage navigation and hero copy following the review notes.')},
  {key:'r1',role:'assistant',kind:'reasoning',text:T('评审意见有三条：导航层级太深、首屏标题过长、移动端按钮换行。我先改结构，再补测试。','Three review points: the navigation nests too deep, the hero title runs long, and buttons wrap on mobile. Fix structure first, then tests.')},
  {key:'t1',role:'activity',kind:'toolCall',title:'Read: src/pages/Home.tsx',text:T('读取 214 行：导航树、首屏组件与按钮组。','Read 214 lines: navigation tree, hero section and button group.')},
  {key:'t2',role:'activity',kind:'toolCall',title:'Bash: npm run lint',exitCode:0,text:'> website@1.4.0 lint\n> eslint src --max-warnings 0\n\n✓ 0 problems (0 errors, 0 warnings)'},
  {key:'p1',role:'activity',kind:'plan',title:T('计划','Plan'),entries:[{content:T('合并二级导航，只留一层下拉','Flatten the second-level navigation'),status:'completed'},{content:T('首屏标题压到 14 字以内','Shorten the hero title to one line'),status:'completed'},{content:T('修正移动端按钮换行','Stop buttons from wrapping on mobile'),status:'in_progress'}]},
  {key:'a1',role:'assistant',text:T('已按评审意见改完结构：\n\n- 导航从三级并成一级，**设置**收进右侧菜单\n- 首屏标题改为「你的工作台，不止在电脑前。」\n- `HeroActions` 在 360px 宽度下改为竖排\n\n下一步补一条移动端快照测试，然后提交。','Structure is done:\n\n- Navigation is one level deep; **Settings** moved into the right menu\n- Hero title is now one line\n- `HeroActions` stacks below 360px\n\nNext: add a mobile snapshot test, then commit.')},
  {key:'u2',role:'user',text:T('好，顺便把按钮的 hover 态也统一一下，改完直接提交。','Good. Unify the button hover state too, then commit.')},
  {key:'a2',role:'assistant',text:T('收到。补齐 `:hover` / `:focus-visible` 后跑一遍测试，再提交到 `main`。','Will do. Adding `:hover` / `:focus-visible`, running tests, then committing to `main`.')}
 ]],
 [ids[2],[
  {key:'u1',role:'user',text:T('把账号池的额度巡检结果汇总一下。','Summarize the account pool quota check.')},
  {key:'a1',role:'assistant',text:T('三个池都能读到额度，CodeBuddy 国内版今天已签到。','All three pools report quota; CodeBuddy CN is checked in for today.')}
 ]]
]);
const skills=[{id:'review',name:'review',displayName:'Review',description:T('按评审清单核对改动','Check changes against a review list')},{id:'docs',name:'docs',displayName:'Docs',description:T('同步更新项目文档','Update project documentation')}];
const catalog=m=>({currentModel:m.model,currentEffort:m.effort,currentServiceTier:m.serviceTier,modelSource:'acp',fastMode:{allowed:true,defaultServiceTier:'default'},models:[{id:'gpt-6.1-sol',name:'GPT-6.1 Sol',efforts:['low','medium','high','xhigh'],defaultEffort:'high',fastTier:'priority'},{id:'gpt-6-astra',name:'GPT-6 Astra',efforts:['low','medium','high'],defaultEffort:'high'},{id:'claude-sonnet-4-6',name:'Claude Sonnet 4.6',efforts:['low','medium','high'],defaultEffort:'high'}],skills,total:skills.length,errors:[]});
const team={id:teamId,name:T('website 团队','website team'),project:sessions[0].projectKey,projectCwd:sessions[0].cwd,defaultForProject:true,leadId:'lead',reviewerId:'reviewer',version:4,
 members:[{id:'lead',sessionId:ids[0],agentKind:'codex',name:'Codex',role:'lead',enabled:true,available:true},{id:'builder',sessionId:ids[2],agentKind:'kimi',name:'Kimi',role:'member',enabled:true,available:true},{id:'reviewer',sessionId:ids[1],agentKind:'claude',name:'Claude',role:'reviewer',enabled:true,available:true}],
 policy:{maxParallelTasks:3,minParallelTasks:1,maxProviderConcurrency:2,maxTaskRetries:1,maxRounds:4,maxTasksPerRun:20,maxContextChars:24000,maxResultChars:8000,taskTimeoutSecs:900,runTimeoutSecs:5400,budgetUnits:60,workspaceMode:'shared_workspace',writePolicy:'exclusive',autoDispatch:true,autoApplyChanges:false}};
const run={id:runId,teamId,goal:T('按评审意见完成首页改版并提交','Ship the homepage revision from the review notes'),mode:'team',collectPolicy:'all',autoPlan:true,status:'running',round:1,planHash:'demo',
 tasks:[
  {id:'lead',memberId:'lead',status:'completed',prompt:T('梳理评审意见，拆成可执行任务','Turn the review notes into executable tasks'),instructionsHead:T('梳理评审意见','Read the review notes'),result:T('拆成三条：导航、首屏、移动端按钮','Three tasks: navigation, hero, mobile buttons')},
  {id:'t1',memberId:'builder',status:'completed',dependsOn:['lead'],prompt:T('合并导航层级并精简首屏标题','Flatten navigation and shorten the hero'),instructionsHead:T('合并导航层级','Flatten navigation'),result:T('导航已合并，首屏标题压到一行','Done; the hero title fits one line')},
  {id:'t2',memberId:'reviewer',status:'running',dependsOn:['t1'],prompt:T('评审改动与可访问性','Review the change and accessibility'),instructionsHead:T('评审改动与可访问性','Review change and accessibility')},
  {id:'t3',memberId:'builder',status:'planned',dependsOn:['t2'],prompt:T('按评审意见修订并补测试','Apply review feedback and add tests'),instructionsHead:T('按评审意见修订','Apply review feedback')}
 ],
 threads:[{id:'th-1',memberId:'lead',agentKind:'codex',providerSessionId:ids[0]},{id:'th-2',memberId:'builder',agentKind:'kimi',providerSessionId:ids[2]},{id:'th-3',memberId:'reviewer',agentKind:'claude',providerSessionId:ids[1]}],
 createdAt:now-9*60000,updatedAt:now-30000};
const accountsData={accounts:[
  {id:'official',name:T('工作账号','Work account'),kind:'chatgpt',email:'work@example.com',planType:'Plus',details:{usage:{status:'ready',checkedAt:now/1000,limits:[{name:'Codex',windows:[{windowDurationMins:300,remainingPercent:78},{windowDurationMins:10080,remainingPercent:61}]}],resetCredits:{availableCount:2,credits:null}}}},
  {id:'backup',name:T('备用账号','Backup account'),kind:'chatgpt',email:'backup@example.com',planType:'Plus',details:{usage:{status:'ready',checkedAt:now/1000,limits:[{name:'Codex',windows:[{windowDurationMins:300,remainingPercent:44},{windowDurationMins:10080,remainingPercent:69}]}],resetCredits:{availableCount:1,credits:null}}}},
  {id:'api',name:T('自建 API','Self-hosted API'),kind:'api',baseUrl:'https://api.example.com/v1',model:'gemini-2.5-pro'}
],current:{status:'ready',kind:'chatgpt',id:'official',name:T('工作账号','Work account')},activeId:'official',switch:{phase:'idle'},canSwitch:true,codexHome:'/Users/demo/.codex'};
const response=data=>new Response(JSON.stringify(data),{headers:{'Content-Type':'application/json'}});
const unsupported=()=>new Response(JSON.stringify({error:T('此操作未在演示中开放；不会修改真实数据。','This action is unavailable in the demo; no real data is changed.')}),{status:400});
const view=id=>({...sessions.find(session=>session.id===id),...meta.get(id)});
const page=id=>{const seq=sequence++;lastSequence.set(id,seq);return {epoch:'demo',sequence:seq,rows:(rows.get(id)||[]).map((row,index)=>({...row,order:index,version:String(index)})),hasMore:false,before:null,files:[],meta:view(id)};};
window.demoIds=ids;window.demoTeams=[team];window.demoTeam=team;
window.fetch=async(input,options={})=>{
 const url=new URL(input,location.href),p=url.pathname,body=typeof options.body==='string'?JSON.parse(options.body):{},match=/sessions\/([^/]+)/.exec(p),id=match?.[1]||ids[0],m=meta.get(id)||meta.get(ids[0]);
 if(options.signal?.aborted)throw new DOMException('Aborted','AbortError');
 if(p==='/api/auth')return response({authenticated:true,csrf:'demo',instanceId:'demo',passwordless:false,transport:'poll',computer:{name:'demo · Mac',platform:'Darwin'}});
 if(p==='/api/projects')return response({projects:[{key:'local|website',name:'website',cwd:'/projects/website',host:'local'},{key:'local|ops',name:'ops',cwd:'/projects/ops',host:'local'}]});
 if(p==='/api/sessions'&&options.method==='POST'){const created=crypto.randomUUID();sessions.unshift({...sessions[0],id:created,title:body.title||T('新聊天','New chat')});meta.set(created,{...baseMeta(sessions[0]),requests:[],submissions:[]});rows.set(created,[]);return response({id:created,host:'local',message:T('聊天已创建','Chat created')});}
 if(p==='/api/sessions')return response({sessions:sessions.filter(session=>session.title.toLowerCase().includes((url.searchParams.get('q')||'').toLowerCase())),unavailableHosts:[]});
 if(p==='/api/activity')return response({sessions:sessions.map(session=>({...session,status:'idle',connected:true,turnStatus:'completed'}))});
 if(p==='/api/teams')return response({teams:[team]});
 if(p==='/api/teams/'+teamId)return response(team);
 if(p==='/api/teams/'+teamId+'/runs')return response({runs:[{id:run.id,status:run.status,createdAt:run.createdAt}]});
 if(p==='/api/teams/'+teamId+'/runs/'+run.id)return response(run);
 if(p==='/api/teams/workspaces')return response({workspaces:[]});
 if(p==='/api/accounts')return response(await window.fixtureAccountRequest({action:'list'}));
 if(p==='/api/accounts/details')return response(await window.fixtureAccountRequest({action:'details',...body}));
 if(p==='/api/accounts/switch')return response(await window.fixtureAccountRequest({action:'switch',...body}));
 if(p==='/api/accounts/ignore-submission')return response({ok:true});
 if(p==='/api/account')return response(window.fixtureUsage());
 if(p==='/api/jev')return response({configured:true,apiKeyMasked:'sk-…4f2a',baseUrl:'https://api.example.com/v1',model:'jev-guard-1',proxy:'',autoApprove:true,autoContinue:false,threshold:0.6,policy:[{name:'commandExecution',polarity:'negative'},{name:'fileChange',polarity:'negative'},{name:'read',polarity:'positive'}]});
 if(p==='/api/jev/trial')return response({ok:true,riskLabel:body.preset==='fileChange'?'low':'elevated',confidence:0.74,reversible:0.9,reason:'',summary:T('只读命令可自动放行；写操作与推送仍会等待确认。','Read-only commands pass automatically; writes and pushes still wait for approval.')});
 if(p==='/api/conventions')return response({artifactPaths:true});
 if(p==='/api/notifications/defaults')return response({requests:true,completion:false});
 if(p==='/api/pool/kimi')return response({accounts:[{id:'kimi-1',label:'Kimi 主号',active:true,status:'active',quota:{membership:{tier:'Andante'},windows:[{windowDurationMins:300,remainingPercent:64}]}}],autoSwitch:true,history:[]});
 if(p==='/api/pool/zcode')return response({accounts:[{id:'zcode-1',label:'z.ai 账号',active:true,status:'active',quota:{resetCredits:{availableCount:2,fiveHourCards:1,weekCards:1}}}],autoSwitch:true,history:[]});
 if(p==='/api/pool/minimax')return response({accounts:[{id:'minimax-1',label:T('MiniMax 国内版','MiniMax'),region:'cn',active:true,status:'active'}],autoSwitch:true,history:[]});
 if(p==='/api/pool/workbuddy')return response({accounts:[{id:'workbuddy-1',label:T('CodeBuddy 国内版','CodeBuddy'),nickname:T('工作号','Work ID'),active:true,status:'active',quota:{membership:{tier:T('国内版','China')},credits:{resources:[{packageName:T('每日积分','Daily credits'),total:2000,remaining:1460}]}},checkinAt:now-3*3600000,checkinNote:T('签到成功，+100 积分','Checked in, +100 credits')}],autoSwitch:true,history:[]});
 if(p.startsWith('/api/pool/'))return response({accounts:[],autoSwitch:true,history:[]});
 if(p.endsWith('/timeline'))return response(page(id));
 if(p.endsWith('/changes'))return response({epoch:'demo',sequence:lastSequence.get(id)||0,rows:[],hasMore:false,before:null,files:[],meta:view(id)});
 if(p.endsWith('/catalog')||p.endsWith('/skills'))return response(catalog(m));
 if(p.endsWith('/permissions')){m.permissionMode=body.preset;return response({confirmed:true});}
 if(p.endsWith('/settings')){Object.assign(m,{model:body.model||m.model,effort:body.effort||m.effort});return response({confirmed:true,applied:true});}
 if(p.endsWith('/send')){rows.get(id).push({key:crypto.randomUUID(),role:'user',text:body.text},{key:crypto.randomUUID(),role:'assistant',text:T('收到。这是示例数据，不会调用模型。','Received. This is sample data; no model is called.')});return response({status:'accepted',id:body.id});}
 if(p.endsWith('/queue')){m.submissions=[...(m.submissions||[]),{id:body.id,text:body.text,mode:'queue',status:'queued'}];return response({status:'queued'});}
 if(p.endsWith('/stop'))return response({ok:true});
 if(p.endsWith('/respond')){m.requests=[];return response({ok:true});}
 if(p.endsWith('/rename')){m.title=body.title;const row=sessions.find(session=>session.id===id);if(row)row.title=body.title;return response({title:body.title});}
 if(p.endsWith('/delete'))return response({mode:'deleted'});
 if(p.endsWith('/notifications'))return response({available:true,watching:false,notifyOnCompletion:false});
 if(p.endsWith('/reconnect')||p.endsWith('/history'))return response({ok:true});
 if(p.endsWith('/migrate'))return response({id:crypto.randomUUID(),host:'local',sent:false,warning:T('演示不会真的迁移会话。','The demo never migrates a real session.')});
 return unsupported();
};
})();
