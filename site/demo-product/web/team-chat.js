'use strict';

function teamSubmissionFields(mode,teamID){
  return mode==='direct'?{teamMode:'direct'}:{teamMode:'team',teamId:teamID};
}

// Lead 动作信封（internal/team/actions.go §5.1）是编排协议，不是给人读的正文：
// 与网关 ExtractActionBlock 同一规则——整段恰好一个成对块（取首个闭合标记之前
// 最后一个开始标记）、能解析成 {actions:[...]}、且不在代码块里，才切成 action
// 片段；多个块（网关整体拒绝）、孤立标记、流式半截块、代码里的协议示例与非法
// JSON 一律保持原文，宁可原样显示也不把未执行的动作画成卡片。codeLines 可选：
// 返回落在代码块（围栏/缩进/引用内）里的 0 基行号集合，markdown.js 用 token.map
// 提供；缺省按行首围栏粗略推断。
function teamActionsCodeLines(source){
  const lines=new Set();let inside=false;
  source.split('\n').forEach((line,index)=>{
    const fence=/^(?:\s*>)*\s*(```|~~~)/.test(line);
    if(fence||inside||/^(?: {4}|\t)/.test(line))lines.add(index);
    if(fence)inside=!inside;
  });
  return lines;
}
function splitTeamActions(text,codeLines){
  const source=String(text||''),open='<team-actions>',close='</team-actions>',whole=[{type:'text',text:source}];
  const firstOpen=source.indexOf(open),firstClose=source.indexOf(close);
  if(firstOpen<0||firstClose<0||firstClose<firstOpen)return whole;
  const rest=source.slice(firstClose+close.length);
  if(rest.includes(open)||rest.includes(close))return whole;
  const start=source.lastIndexOf(open,firstClose),end=firstClose+close.length;
  const lineOf=offset=>source.slice(0,offset).split('\n').length-1;
  const code=(codeLines||teamActionsCodeLines)(source);
  if(code.has(lineOf(start))||code.has(lineOf(firstClose)))return whole;
  let actions=null;
  try{const value=JSON.parse(source.slice(start+open.length,firstClose));if(value&&Array.isArray(value.actions))actions=value.actions;}catch{}
  if(!actions)return whole;
  return [{type:'text',text:source.slice(0,start)},{type:'actions',actions,raw:source.slice(start,end)},{type:'text',text:rest}]
    .filter(part=>part.type==='actions'||part.text.trim());
}

const TEAM_ACTIONS_CARD_LABELS={delegate:'委派任务',request_context:'请求上下文',summarize:'请求汇总'};
function renderTeamActionsCard(part,translate){
  const t=translate||(value=>value),make=(tag,text,className)=>{const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;};
  const card=make('div',undefined,'team-actions-card');
  card.append(make('strong',t('团队动作')+' · '+(part.actions.length?part.actions.length:t('无需委派'))));
  if(part.actions.length){
    const list=make('ul');
    for(const action of part.actions){
      const item=make('li');
      item.append(make('span',t(TEAM_ACTIONS_CARD_LABELS[action&&action.type]||String(action&&action.type||'')),'team-actions-type'));
      if(action&&action.member)item.append(make('span',String(action.member),'team-actions-member'));
      if(action&&action.writeLock==='none')item.append(make('span',t('只读'),'team-actions-member'));
      if(action&&action.task)item.append(make('p',String(action.task),'team-actions-task'));
      list.append(item);
    }
    card.append(list);
  }
  const raw=make('details');raw.append(make('summary',t('查看原文')),make('pre',part.raw));
  card.append(raw);
  return card;
}

// 交付卡（§8 阶段三）：团队聊天卡与 Run 页共用。Agent 完成度、Review 裁决、验证闸门、
// 待答问题与独立工作区分别落位——Agent 的「完成了」不替代审查或验证。
function renderTeamDeliveryEvidence(container,delivery,make){
  if(!delivery||typeof delivery!=='object')return;
  const evidence=make('div',undefined,'team-delivery-evidence');
  const agent=delivery.agent,review=delivery.review,verify=delivery.verify;
  const reviewText=!review?null:(review.verdict==='passed'?t_safe('已通过'):review.verdict==='failed'?t_safe('未通过'):t_safe('结果未知'));
  const verifyText=!verify?null:(verify.configured===false?t_safe('未配置'):(verify.status==='passed'?t_safe('已通过'):verify.status==='failed'?t_safe('未通过'):t_safe('待验证')));
  const dims=[
    t_safe('Agent')+t_safe('：')+(agent?agent.completed+'/'+agent.total+' '+t_safe('已完成'):'—'),
    t_safe('审查')+t_safe('：')+(reviewText||t_safe('未参与')),
    t_safe('验证')+t_safe('：')+(verifyText||t_safe('未配置'))
  ];
  if(delivery.pendingQuestions>0)dims.push(t_safe('待回答问题')+t_safe('：')+delivery.pendingQuestions);
  for(const dim of dims)evidence.append(make('span',dim,'team-delivery-dim'));
  container.append(evidence);
  // isolated 任务的改动留在独立 worktree，验收前要让用户知道它尚未进入主工作区。
  if(Array.isArray(delivery.workspaces)&&delivery.workspaces.length){
    const names=delivery.workspaces.map(entry=>entry.workspaceId+(entry.taskId?' ('+entry.taskId+')':'')).join(t_safe('、'));
    container.append(make('p',t_safe('独立工作区：')+names+' · '+t_safe('不会自动合入主工作区'),'muted team-delivery-dim'));
  }
  const summary=delivery.resultSummary;
  // 每行一个任务的纯文本结果头（网关已剥 Markdown 标记）。
  if(summary&&summary.text)container.append(make('p',summary.text+(summary.truncated?t_safe('（已截断）'):''),'team-delivery-summary'));
}

// 停靠类 outcome 的站点原因（与 team.js TEAM_TASK_OUTCOME_LABELS 同一词条）。
const TEAM_WORKFLOW_OUTCOME_LABELS={rate_limited:'触发限流',quota:'额度不足',auth:'成员认证失败',not_configured:'成员未配置',context_overflow:'上下文超限',transient:'临时错误'};

function teamWorkflowDependencies(task,byId){
  if(Array.isArray(task.dependsOn)&&task.dependsOn.length)return task.dependsOn;
  const match=/^(.*)-d\d+-\d+$/.exec(task.id||''),source=match&&byId.get(match[1]);
  if(!source)return[];
  const deps=[match[1]];
  if(source.memberId===task.memberId){
    for(const candidate of byId.values()){
      const sibling=/^(.*)-d\d+-\d+$/.exec(candidate.id||'');
      if(candidate.id!==task.id&&candidate.memberId!==task.memberId&&!candidate.reduction&&sibling&&sibling[1]===match[1])deps.push(candidate.id);
    }
  }
  return deps;
}

function teamWorkflowStages(tasks){
  const byId=new Map(tasks.map(task=>[task.id,task])),depths=new Map();
  const depth=(task,visiting=new Set())=>{
    if(!task)return 0;
    if(depths.has(task.id))return depths.get(task.id);
    if(visiting.has(task.id))return 0;
    visiting.add(task.id);
    const deps=teamWorkflowDependencies(task,byId);
    const value=deps.length?1+Math.max(...deps.map(id=>depth(byId.get(id),new Set(visiting)))):0;
    depths.set(task.id,value);return value;
  };
  const stages=[];
  for(const task of tasks){const level=depth(task);(stages[level]||(stages[level]=[])).push(task);}
  return stages.filter(Boolean);
}

// teamWorkflowTaskTitle 取轨道上展示的任务标题：优先提示词首行——任务 id
// 只是内部标识（lead-d1-0 这类），对用户没有意义；首行过长交给 CSS 两行截断。
function teamWorkflowTaskTitle(task){
  if(task.id==='lead')return t_safe('负责人');
  const text=String(task.instructionsHead||task.prompt||'');
  let line=(text.split(/\r?\n/).find(row=>row.trim())||'').trim().replace(/^[#>\-*\s]+/,'');
  // 提示词常以绝对路径开头（「在 /Users/… 只读排查 …」）。路径在窄列里会吃掉整行，
  // 显示时按词剔除；换行与省略交给 CSS 两行截断，完整原文仍在 title 里。
  // 逐词判断而不是正则：路径里斜杠多，嵌套量词在长提示词上会回溯到卡死页面。
  line=line.split(/\s+/)
    .filter(token=>token.split('/').length<3||!token.startsWith('/'))
    .map(token=>token.length>26&&/^[\x21-\x7E]+$/.test(token)?token.slice(0,14)+'…'+token.slice(-10):token)
    .join(' ')
    .replace(/^(?:在|对|到|from|at|in)\s+/i,'').trim();
  return line||task.id||t_safe('任务');
}

// teamWorkflowCounts 把任务按展示语义分桶：Run 的 status 只表示编排走到哪，
// 失败/停止要从任务本身数，否则「已完成」会把 2 个失败任务说成成功。
function teamWorkflowCounts(tasks){
  const counts={total:0,completed:0,failed:0,running:0,stopped:0,pending:0};
  for(const task of tasks||[]){
    const status=String(task&&task.status||'planned');
    counts.total++;
    if(status==='completed')counts.completed++;
    else if(status==='failed'||status==='outcome_unknown')counts.failed++;
    else if(status==='running'||status==='dispatching'||status==='ready')counts.running++;
    else if(status==='stopped'||status==='cancelled')counts.stopped++;
    else counts.pending++;
  }
  return counts;
}

// teamWorkflowOutcome 决定头部那枚结论：tone 只有 ok/warn/bad/idle 四档，
// 颜色只表达「好了没有」，不表达进度。
function teamWorkflowOutcome(run,counts){
  const status=String(run&&run.status||''),failed=Number(counts&&counts.failed||0);
  const terminal=typeof TEAM_TERMINAL_RUN_STATUSES!=='undefined'?TEAM_TERMINAL_RUN_STATUSES.has(status):false;
  if(status==='outcome_unknown')return{label:'结果未知',tone:'bad'};
  if(status==='failed')return{label:'已失败',tone:'bad'};
  if(status==='cancelled')return{label:'已取消',tone:'idle'};
  if(status==='stopped')return{label:'已停止',tone:'idle'};
  if(status==='waiting_input')return{label:'等待确认',tone:'warn'};
  if(failed&&(terminal||!status))return{label:'部分失败',tone:'bad'};
  if(status==='created')return{label:'已创建',tone:'idle'};
  if(!terminal)return{label:teamRunStatusLabel(status),tone:'warn'};
  return{label:'已完成',tone:'ok'};
}

// teamWorkflowMemberLabel 避免「kimi · Kimi」这种同一句话写两遍：
// 成员名与 Agent 名相同时只留一个。
function teamWorkflowMemberLabel(member,task){
  const owner=member||{},name=String(owner.name||owner.id||(task&&task.memberId)||'').trim();
  const agent=owner.agentKind?BridgeI18n.agentLabel(owner.agentKind):'';
  if(!name)return agent||t_safe('成员');
  if(!agent||name.toLowerCase()===String(agent).toLowerCase())return name;
  return name+' · '+agent;
}

class TeamChatController{
  constructor({root,request,openRun,onChange}){
    Object.assign(this,{root,request,openRun,onChange});
    this.teams=[];this.current=null;this.run=null;this.team=null;
    this.pending=false;this.queued=false;this.lastFetch=0;this.generation=0;this.compact=false;
  }
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  button(text,fn){const node=this.node('button',text);node.type='button';node.onclick=fn;return node;}
  setTeams(teams){this.teams=Array.isArray(teams)?teams:[];}
  teamById(id){return this.teams.find(team=>team.id===id)||null;}
  teamForProject(project){
    const key=typeof project==='string'?project:project.key,cwd=typeof project==='string'?'':project.cwd;
    return this.teams.find(team=>team.defaultForProject&&(team.project===key||(cwd&&team.projectCwd===cwd)))
      ||this.teams.find(team=>team.project===key||(cwd&&team.projectCwd===cwd))||null;
  }
  teamForSession(sessionId){
    const matches=this.teams.filter(team=>(team.members||[]).some(member=>member.id===team.leadId&&member.sessionId===sessionId));
    return matches.length===1?matches[0]:null;
  }
  open(team){
    clearTimeout(this.timer);
    this.current=team||null;this.team=team||null;this.run=null;this.compact=false;this.generation++;
    this.refreshError='';
    this.render();if(this.current)this.refreshRun(true);
  }
  close(){this.open(null);}
  openSettings(){if(this.current)new TeamSettingsDialog({request:this.request,onChange:team=>{this.onChange&&this.onChange(team);}}).open(this.current);}
  accepted(){this.refreshRun(true);}
  schedule(){
    clearTimeout(this.timer);if(!this.current)return;
    if(this.run&&TEAM_TERMINAL_RUN_STATUSES.has(this.run.status))return;
    this.timer=setTimeout(()=>this.refreshRun(true),4000);this.timer.unref?.();
  }
  async refreshRun(force=false){
    const team=this.current;
    if(!team)return;
    const now=Date.now();
    if(!force&&now-this.lastFetch<4000)return;
    if(this.pending){this.queued=true;return;}
    this.pending=true;this.queued=false;this.lastFetch=now;
    const generation=this.generation;
    try{
      const listed=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/runs');
      const rows=listed.runs||[],latest=rows[rows.length-1];
      if(!latest){if(generation===this.generation){this.run=null;this.refreshError='';this.render();}return;}
      const detail=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/runs/'+encodeURIComponent(latest.id));
      if(generation===this.generation&&this.current?.id===team.id){this.team=team;this.run=detail;this.refreshError='';this.render();}
    }catch(error){
      // 临时失败保留已有卡片（否则运行中的团队会闪回「从没跑过」的引导），只提示刷新失败。
      if(generation===this.generation&&this.current?.id===team.id){this.refreshError=error.message;this.render();}
    }finally{
      this.pending=false;
      if(this.queued&&this.current)this.refreshRun(true);else this.schedule();
    }
  }
  render(){
    const root=this.root,team=this.team,run=this.run;
    root.replaceChildren();
    if(!this.current||!team){root.hidden=true;return;}
    root.hidden=false;
    if(!run){
      const empty=this.node('section',undefined,'team-workflow-empty');
      empty.append(this.node('strong',t_safe('团队已就绪，给团队发第一条消息')));
      const roster=this.node('div',undefined,'team-workflow-roster');
      for(const member of team.members||[]){
        const row=this.node('span',undefined,'team-workflow-roster-member');
        const dot=this.node('i',undefined,'team-workflow-agent');
        if(member.agentKind)dot.setAttribute('data-agent',member.agentKind);
        row.append(dot,this.node('span',teamWorkflowMemberLabel(member)));
        roster.append(row);
      }
      empty.append(roster,this.node('p',t_safe('发送后会在这里显示阶段、成员和结果。'),'team-workflow-meta'));
      if(this.refreshError)empty.append(this.node('p',t_safe('刷新失败：')+this.refreshError,'error'));
      root.append(empty);return;
    }
    const tasks=Array.isArray(run.tasks)?run.tasks:[],members=new Map((team.members||[]).map(member=>[member.id,member]));
    const counts=teamWorkflowCounts(tasks),outcome=teamWorkflowOutcome(run,counts);
    const card=this.node('section',undefined,'team-workflow-card'+(this.compact?' compact':''));
    card.setAttribute('aria-label',t_safe('Workflow 运行摘要'));
    const head=this.node('div',undefined,'team-workflow-head');
    const headline=this.node('strong',undefined,'team-workflow-headline');
    headline.append(this.node('span','Workflow'));
    const badge=this.node('span',t_safe(outcome.label),'team-workflow-outcome');
    badge.setAttribute('data-tone',outcome.tone);
    // 与其它团队徽标一致：切语言时 BridgeI18n.apply 能就地刷新这枚结论。
    badge.setAttribute('data-i18n',outcome.label);
    headline.append(badge);
    head.append(headline);
    // 计数从任务本身算：Run 说「已完成」时也可能有失败任务，头部必须把它们说出来。
    const meta=this.node('span',undefined,'team-workflow-meta');
    meta.append(this.node('span',counts.total+' '+t_safe('个任务')));
    // 所有非零计数都说出来：失败不能盖住真正卡住流程的停靠任务。
    if(counts.failed)meta.append(this.node('span',counts.failed+' '+t_safe('失败'),'team-workflow-failcount'));
    if(counts.running)meta.append(this.node('span',counts.running+' '+t_safe('进行中'),'team-workflow-runcount'));
    if(counts.stopped)meta.append(this.node('span',counts.stopped+' '+t_safe('个停止')));
    head.append(meta);
    const toggle=this.button(this.compact?t_safe('展开成员'):t_safe('收起成员'),()=>{this.compact=!this.compact;this.render();});
    toggle.setAttribute('aria-expanded',String(!this.compact));
    head.append(toggle);
    head.append(this.button(t_safe('详情'),()=>this.openRun&&this.openRun(team.id,run.id)));
    card.append(head);
    const scroller=this.node('div',undefined,'team-workflow-scroll');
    scroller.tabIndex=0;scroller.setAttribute('aria-label',t_safe('阶段轨道，可横向滚动'));
    const stations=this.node('div',undefined,'team-workflow-stations');
    const rows=teamWorkflowStages(tasks);
    for(const [index,stageTasks] of rows.entries()){
      const stage=this.node('div',undefined,'team-workflow-stage');
      // 并行阶段内任务多，给宽一点：等宽列会让两行的任务名被硬折在词中间。
      if(stageTasks.length>1)stage.style.flexGrow=String(Math.min(stageTasks.length,3));
      const caption=this.node('div',undefined,'team-workflow-stage-head');
      caption.append(this.node('span',t_safe('阶段')+' '+(index+1),'team-workflow-stage-index'));
      if(stageTasks.length>1)caption.append(this.node('span',t_safe('并行')+' '+stageTasks.length,'team-workflow-stage-parallel'));
      stage.append(caption);
      const lanes=this.node('div',undefined,'team-workflow-lanes');
      for(const [position,task] of stageTasks.entries()){
        const member=members.get(task.memberId)||{},status=task.status||'planned';
        const failed=status==='failed'||status==='outcome_unknown';
        const chain=position<stageTasks.length-1?' continued':(index<rows.length-1?' chain':'');
        const station=this.node('div',undefined,'team-workflow-station '+status+chain);
        station.title=String(task.instructionsHead||task.prompt||'').trim();
        station.setAttribute('aria-label',teamWorkflowTaskTitle(task)+' · '+t_safe(teamTaskStatusLabel(status)));
        station.append(this.node('span',undefined,'team-workflow-lamp'));
        const body=this.node('div',undefined,'team-workflow-body');
        body.append(this.node('span',teamWorkflowTaskTitle(task),'team-workflow-step'));
        const who=this.node('span',undefined,'team-workflow-member');
        const dot=this.node('i',undefined,'team-workflow-agent');
        if(member.agentKind)dot.setAttribute('data-agent',member.agentKind);
        who.append(dot,this.node('span',teamWorkflowMemberLabel(member,task),'team-workflow-member-label'));
        body.append(who);
        // 失败/停靠的站点直接说出原因（Run 详情里也是这句），省掉一次跳转；全文放 title。
        const outcome=status==='stopped'&&TEAM_WORKFLOW_OUTCOME_LABELS[task.outcome];
        const reason=failed?t_safe(String(task.error||'').trim()):outcome?t_safe(outcome):'';
        if(reason){const line=this.node('span',reason,'team-workflow-reason');line.title=reason;body.append(line);}
        station.append(body);
        lanes.append(station);
      }
      stage.append(lanes);
      stations.append(stage);
    }
    scroller.append(stations);card.append(scroller);
    if(run.verify){
      const artifacts=this.node('div',undefined,'team-workflow-artifacts');
      artifacts.append(this.node('span',t_safe('验证退出码')+' '+run.verify.exitCode,'team-workflow-artifact'));
      card.append(artifacts);
    }
    // 完成的 Run 只展示证据，不设验收：成果已在工作区，有问题直接在团队聊天继续反馈。
    if(run.status==='completed'&&run.delivery){
      const delivery=this.node('div',undefined,'team-delivery');
      renderTeamDeliveryEvidence(delivery,run.delivery,(tag,text,cls)=>this.node(tag,text,cls));
      const actions=this.node('div',undefined,'team-delivery-actions');
      // 修订入口在 Run 页（三段流对话框），这里只负责带过去。
      actions.append(this.button(t_safe('去 Run 页修订'),()=>this.openRun&&this.openRun(team.id,run.id)));
      delivery.append(actions);
      card.append(delivery);
    }
    if(this.refreshError)card.append(this.node('p',t_safe('刷新失败：')+this.refreshError,'error'));
    root.append(card);
  }
}
class TeamLeadPicker{
  constructor({request}){this.request=request;this._dialog=null;}
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  dialog(){
    if(this._dialog)return this._dialog;
    const dialog=document.createElement('dialog');dialog.className='picker team-lead-picker';
    const head=this.node('div',undefined,'picker-head');
    head.append(this.node('h2',t_safe('选择主控 Agent')));
    const close=this.node('button','×','icon-button');close.type='button';close.setAttribute('aria-label',t_safe('关闭'));close.onclick=()=>dialog.close();head.append(close);
    this.body=this.node('div',undefined,'picker-body');dialog.append(head,this.body);document.body.append(dialog);this._dialog=dialog;return dialog;
  }
  async open(project){
    const value=await this.request('/api/projects/'+encodeURIComponent(project.key)+'/agents'),agents=value.agents||[];
    if(!agents.length)throw new Error(t_safe('项目中还没有可用的 ACP Agent，请先在该项目使用一个 Agent'));
    const dialog=this.dialog();
    return new Promise(resolve=>{
      this.body.replaceChildren(this.node('p',project.name+t_safe(' 的默认团队尚未创建。请选择首次主控，之后可在团队设置中更换。'),'muted'));
      for(const agent of agents){
        const button=this.node('button',agent.agent,'team-lead-choice');
        button.type='button';button.onclick=()=>{resolve(agent.agent);dialog.close();};this.body.append(button);
      }
      const cancel=this.node('button',t_safe('取消'),'plain');cancel.type='button';cancel.onclick=()=>dialog.close();this.body.append(cancel);
      dialog.addEventListener('close',()=>resolve(null),{once:true});dialog.showModal();
    });
  }
}

class TeamSettingsDialog{
  constructor({request,onChange}){this.request=request;this.onChange=onChange;this._dialog=null;this.team=null;this.agents=[];this.busy=false;this.error='';}
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  button(text,fn){const node=this.node('button',text);node.type='button';node.onclick=fn;return node;}
  dialog(){
    if(this._dialog)return this._dialog;
    const dialog=document.createElement('dialog');dialog.className='picker team-settings-dialog';
    const head=this.node('div','', 'picker-head'),title=this.node('h2',t_safe('团队设置')),close=this.button('×',()=>dialog.close());
    close.className='icon-button';close.setAttribute('aria-label',t_safe('关闭'));head.append(title,close);
    this.body=this.node('div',undefined,'picker-body');dialog.append(head,this.body);document.body.append(dialog);this._dialog=dialog;return dialog;
  }
  async open(team){
    const dialog=this.dialog();this.team=team;this.error='';this.render();dialog.showModal();
    try{
      const detail=await this.request('/api/teams/'+encodeURIComponent(team.id));this.team=detail;
      if(detail.project){
        try{const value=await this.request('/api/projects/'+encodeURIComponent(detail.project)+'/agents');this.agents=value.agents||[];}catch{this.agents=[];}
      }
    }catch(error){this.error=error.message;}
    this.render();
  }
  render(){
    if(!this.body)return;
    this.body.replaceChildren();
    if(!this.team){this.body.append(this.node('p',t_safe('正在读取团队…'),'muted'));return;}
    const team=this.team,members=team.members||[];
    const roles=this.node('section',undefined,'team-settings-section team-role-grid');
    const leadField=this.node('label',undefined,'team-role-card');
    leadField.append(this.node('span',t_safe('主控 Agent'),'team-role-label'));
    const leadSelect=document.createElement('select');leadSelect.className='team-role-select';leadSelect.disabled=this.busy;
    for(const member of members)leadSelect.append(new Option(BridgeI18n.agentLabel(member.agentKind)+' · '+member.agentKind,member.id));
    leadSelect.value=team.leadId||'';leadSelect.onchange=()=>this.changeLead(leadSelect.value);
    leadField.append(leadSelect);
    const reviewerField=this.node('label',undefined,'team-role-card');
    reviewerField.append(this.node('span',t_safe('默认评审 Agent'),'team-role-label'));
    const reviewerSelect=document.createElement('select');reviewerSelect.className='team-role-select';reviewerSelect.disabled=this.busy;
    reviewerSelect.append(new Option(t_safe('不设置默认评审'),''));
    for(const member of members)reviewerSelect.append(new Option(BridgeI18n.agentLabel(member.agentKind)+' · '+member.agentKind,member.id));
    reviewerSelect.value=team.reviewerId||(members.find(member=>member.role==='reviewer')?.id)||'';
    reviewerSelect.onchange=()=>this.changeReviewer(reviewerSelect.value);
    reviewerField.append(reviewerSelect);roles.append(leadField,reviewerField);this.body.append(roles);
    const memberSection=this.node('section',undefined,'team-settings-section');
    memberSection.append(this.node('h3',t_safe('成员')));
    for(const member of members){
      const row=this.node('div',undefined,'team-settings-member');
      const info=this.node('div',undefined,'team-member-info');
      info.append(this.node('strong',BridgeI18n.agentLabel(member.agentKind)+' · '+(member.name&&member.name!==member.agentKind?member.name:member.agentKind)));
      const roleLabels=[];
      if(member.id===team.leadId)roleLabels.push(t_safe('主控'));
      if(member.id===team.reviewerId||member.role==='reviewer')roleLabels.push(t_safe('评审'));
      if(!roleLabels.length)roleLabels.push(t_safe('成员'));
      info.append(this.node('span',roleLabels.join(' · ')+(member.available===false?' · '+t_safe('不可用'):''),'team-member-role'));
      row.append(info);
      if(member.id!==team.leadId){const remove=this.button(t_safe('移除'),()=>this.removeMember(member.id));remove.disabled=this.busy;remove.className='team-member-remove';row.append(remove);}
      memberSection.append(row);
    }
    // 同一 Agent 允许出现多名成员（主控兼任兜底成员）：只按成员 id/会话去重，
    // 不按 agent kind 去重。
    const candidates=this.agents;
    const select=document.createElement('select');select.className='team-add-agent';select.disabled=this.busy;
    for(const agent of candidates)select.append(new Option(agent.agent,agent.agent));
    const role=document.createElement('select');role.className='team-add-role';role.disabled=this.busy;role.append(new Option(t_safe('成员'),'member'),new Option(t_safe('评审'),'reviewer'));
    const add=this.button(t_safe('＋ 添加 Agent'),()=>this.addMember(select.value,role.value));
    add.disabled=this.busy||!candidates.length;
    const addRow=this.node('div',undefined,'team-settings-add');addRow.append(select,role,add);memberSection.append(addRow);
    this.body.append(memberSection);
    if(this.error)this.body.append(this.node('p',this.error,'error'));
  }
  async changeLead(memberID){
    if(memberID===this.team.leadId)return;
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id),{method:'PATCH',body:{leadId:memberID,version:this.team.version}}));
  }
  async changeReviewer(memberID){
    const current=this.team.reviewerId||(this.team.members||[]).find(member=>member.role==='reviewer')?.id||'';
    if(memberID===current)return;
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id),{method:'PATCH',body:{reviewerId:memberID,version:this.team.version}}));
  }
  async addMember(agent,role){
    if(!agent)return;
    const base='member-'+agent.replace(/[^A-Za-z0-9_-]/g,'-');let id=base,index=2;
    while((this.team.members||[]).some(member=>member.id===id))id=base+'-'+(index++);
    // 同名 Agent 的多名成员用后缀区分，便于主控在成员目录里认人。
    const sameKind=(this.team.members||[]).filter(member=>member.agentKind===agent).length;
    const name=sameKind?agent+'-'+(sameKind+1):agent;
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id)+'/members',{method:'POST',body:{id,agent,role,name,version:this.team.version}}));
  }
  async removeMember(memberID){
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id)+'/members/'+encodeURIComponent(memberID)+'?version='+this.team.version,{method:'DELETE'}));
  }
  async mutate(operation){
    if(this.busy)return;
    this.busy=true;this.error='';this.render();
    try{this.team=await operation();this.onChange&&this.onChange(this.team);}
    catch(error){this.error=error.message;}
    finally{this.busy=false;this.render();}
  }
}
if(typeof module!=='undefined')module.exports={splitTeamActions,renderTeamActionsCard,renderTeamDeliveryEvidence,TeamChatController,TeamSettingsDialog,TeamLeadPicker,teamSubmissionFields,teamWorkflowStages,teamWorkflowTaskTitle,teamWorkflowCounts,teamWorkflowOutcome,teamWorkflowMemberLabel};
