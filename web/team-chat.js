'use strict';

// 团队输入队列（docs/team-demand-queue-plan.md §3.2/§7，阶段 4）：运行中
// 发送自动按 Team 开启排队并带 teamQueueVersion:2，先落持久账本
// （202 queued，不给 runId），路由结果由队列面板追；空闲发送保持
// 「立即给 runId」的同步契约。
function teamQueueStorage(){
  try{return globalThis.localStorage;}catch{return null;}
}
function teamQueueModeEnabled(teamID){
  const store=teamQueueStorage();
  return !!store&&teamID&&store.getItem('teamQueueMode:'+teamID)==='1';
}
function teamSubmissionFields(teamID){
  const fields={teamMode:'team',teamId:teamID,teamIssueRoutingVersion:'1'};
  if(teamQueueModeEnabled(teamID))fields.teamQueueVersion='2';
  return fields;
}

// 输入队列状态词条（queueStatus → 展示文案）。
const TEAM_INPUT_STATUS_LABELS={queued:'排队中',routing:'路由中',routed:'已路由',waiting_input:'等待处理',failed:'失败',cancelled:'已取消'};
function teamInputStatusLabel(status){return TEAM_INPUT_STATUS_LABELS[status]||String(status||'');}
function teamInputHead(text){
  const source=String(text||'').trim();
  const row=(source.split(/\r?\n/).find(line=>line.trim())||'').trim();
  return Array.from(row).slice(0,60).join('')+(Array.from(row).length>60?'…':'')||'（附件消息）';
}
function teamQueueOpID(){
  return globalThis.crypto?.randomUUID?.()||'op-'+Date.now()+'-'+Math.random().toString(16).slice(2);
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
  // stale（阶段 5）：审查后又有新写入——旧裁决不再充当当前版本的证据，
  // 独立文案展示，绝不与「已通过」混同。
  const reviewText=!review?null:(review.stale?t_safe('已过期（改动后未重审）'):(review.verdict==='passed'?t_safe('已通过'):review.verdict==='failed'?t_safe('未通过'):t_safe('结果未知')));
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
  constructor({root,request,openRun,onChange,onCompactLead,onResetLead}){
    Object.assign(this,{root,request,openRun,onChange,onCompactLead,onResetLead});
    this.teams=[];this.current=null;this.run=null;this.team=null;
    this.pending=false;this.queued=false;this.lastFetch=0;this.generation=0;this.compact=false;
    this.queue=null;this.queueError='';this.queueExpanded=false;this.excludedRuns=new Map();
  }
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  button(text,fn){const node=this.node('button',text);node.type='button';node.onclick=fn;return node;}
  setTeams(teams){this.teams=Array.isArray(teams)?teams:[];}
  resetWorkflow(teamId,runs){
    const excluded=new Set(this.excludedRuns.get(teamId)||[]);
    for(const run of runs||[])if(run?.id)excluded.add(run.id);
    this.excludedRuns.set(teamId,excluded);
    if(this.current?.id===teamId){this.run=null;this.refreshError='';this.render();}
  }
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
  queueMode(){return teamQueueModeEnabled(this.current?.id);}
  setQueueMode(on){
    const store=teamQueueStorage();
    if(!store||!this.current||this.queueMode()===on)return;
    if(on)store.setItem('teamQueueMode:'+this.current.id,'1');
    else store.removeItem('teamQueueMode:'+this.current.id);
    this.queue=null;this.queueError='';
    this.render();
    if(on)this.refreshQueue();
  }
  async refreshQueue(){
    const team=this.current;
    if(!team||!this.queueMode())return;
    const generation=this.generation;
    try{
      const queue=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/inputs?limit=50');
      if(generation===this.generation&&this.current?.id===team.id){this.queue=queue;this.queueError='';this.render();}
    }catch(error){
      if(generation===this.generation&&this.current?.id===team.id){this.queueError=error.message;this.render();}
    }
  }
  async queueAction(input,action){
    const team=this.current;
    if(!team||!input)return;
    try{
      await this.request('/api/teams/'+encodeURIComponent(team.id)+'/inputs/'+encodeURIComponent(input.inputId)+'/'+action,
        {method:'POST',body:{id:teamQueueOpID(),version:input.version}});
      await this.refreshQueue();
      this.refreshRun(true);
    }catch(error){this.queueError=error.message;this.render();}
  }
  openQueueEdit(input){
    const team=this.current;
    if(!team||!input)return;
    const dialog=document.createElement('dialog');dialog.className='picker team-queue-edit';
    const head=this.node('div',undefined,'picker-head');
    head.append(this.node('h2',t_safe('编辑排队输入')));
    const close=this.node('button','×','icon-button');close.type='button';close.setAttribute('aria-label',t_safe('关闭'));
    close.onclick=()=>dialog.close();head.append(close);
    const body=this.node('div',undefined,'picker-body');
    const editor=document.createElement('textarea');editor.value=input.text||'';editor.rows=6;
    editor.setAttribute('aria-label',t_safe('输入内容'));
    const errorLine=this.node('p','','error');
    const save=this.button(t_safe('保存'),async()=>{
      const text=editor.value.trim();
      if(!text){errorLine.textContent=t_safe('内容不能为空');return;}
      save.disabled=true;
      try{
        await this.request('/api/teams/'+encodeURIComponent(team.id)+'/inputs/'+encodeURIComponent(input.inputId),
          {method:'PATCH',body:{id:teamQueueOpID(),version:input.version,text}});
        dialog.close();
        await this.refreshQueue();
      }catch(error){errorLine.textContent=error.message;save.disabled=false;}
    });
    const cancel=this.button(t_safe('取消'),()=>dialog.close());cancel.className='plain';
    body.append(editor,errorLine,save,cancel);
    dialog.append(head,body);document.body.append(dialog);
    dialog.addEventListener('close',()=>dialog.remove(),{once:true});
    dialog.showModal();
  }
  // 团队运行期间展示输入队列，提供立即发送、编辑与删除操作。
  renderQueuePanel(){
    if(!this.queueMode())return;
    const team=this.team,root=this.root;
    const panel=this.node('section',undefined,'team-queue-panel');
    const head=this.node('div',undefined,'team-queue-head');
    head.append(this.node('strong',t_safe('输入队列')));
    panel.append(head);
    const queue=this.queue;
    if(!queue){
      panel.append(this.node('p',this.queueError?t_safe('读取失败：')+this.queueError:t_safe('读取中…'),'team-queue-hint'));
      root.append(panel);return;
    }
    const meta=this.node('div',undefined,'team-queue-meta');
    const cap=Number(queue.maxPendingInputs||0);
    meta.append(this.node('span',t_safe('待处理')+' '+Number(queue.pendingCount||0)+(cap?t_safe(' · 上限')+' '+cap:'')));
    if(this.queueError)meta.append(this.node('span',t_safe('刷新失败：')+this.queueError,'error'));
    panel.append(meta);
    const inputs=Array.isArray(queue.inputs)?queue.inputs:[];
    if(!inputs.length){panel.append(this.node('p',t_safe('队列为空：发送新消息会立即受理。'),'team-queue-hint'));root.append(panel);return;}
    const rows=this.queueExpanded?inputs:inputs.slice(0,5);
    const list=this.node('div',undefined,'team-queue-list');
    for(const input of rows){
      const row=this.node('div',undefined,'team-queue-row');
      row.dataset.queueStatus=input.queueStatus||'';
      const line=this.node('div',undefined,'team-queue-line');
      const status=input.queueStatus||'queued';
      line.append(this.node('span','#'+Number(input.sequence||0),'team-queue-seq'));
      line.append(this.node('span',teamInputHead(input.text),'team-queue-text'));
      if(input.priority)line.append(this.node('span','⚡','team-queue-priority'));
      const badge=this.node('span',t_safe(teamInputStatusLabel(status)),'team-queue-status');
      badge.dataset.status=status;
      line.append(badge);
      row.append(line);
      if(input.waitingReason)row.append(this.node('p',input.waitingReason,'team-queue-reason'));
      const details=this.node('div',undefined,'team-queue-details');
      const runID=input.targetRunId||(Array.isArray(input.runIds)&&input.runIds[input.runIds.length-1])||'';
      if(runID){
        const link=this.button(t_safe('Run')+' '+runID.slice(0,8),()=>this.openRun&&this.openRun(team.id,runID));
        link.className='team-queue-run';details.append(link);
      }
      const items=Array.isArray(input.workItems)?input.workItems:[];
      if(items.length)details.append(this.node('span',t_safe('工作项')+' '+items.length,'team-queue-workitems'));
      const revised=Number(input.revision||1)>1;
      if(revised)details.append(this.node('span',t_safe('第')+Number(input.revision||1)+t_safe('次修订'),'team-queue-revision'));
      row.append(details);
      if(status==='queued'){
        const actions=this.node('div',undefined,'team-queue-actions');
        if(!input.priority)actions.append(this.button(t_safe('立即发送'),()=>this.queueAction(input,'prioritize')));
        actions.append(this.button(t_safe('编辑'),()=>this.openQueueEdit(input)));
        actions.append(this.button(t_safe('删除'),()=>this.queueAction(input,'cancel')));
        row.append(actions);
      }else if(status==='waiting_input'||status==='routing'){
        row.append(this.node('p',t_safe('已进入路由，排队操作不可用；运行中的修订请发送新消息。'),'team-queue-reason'));
      }
      list.append(row);
    }
    panel.append(list);
    if(inputs.length>5){
      const more=this.button(this.queueExpanded?t_safe('收起'):t_safe('展开全部')+' ('+inputs.length+')',()=>{this.queueExpanded=!this.queueExpanded;this.render();});
      more.className='team-queue-more';panel.append(more);
    }
    root.append(panel);
  }
  open(team){
    clearTimeout(this.timer);
    this.current=team||null;this.team=team||null;this.run=null;this.compact=false;this.generation++;
    this.refreshError='';this.queue=null;this.queueError='';this.queueExpanded=false;
    this.render();if(this.current){this.refreshRun(true);this.refreshQueue();}
  }
  close(){this.open(null);}
  openSettings(){if(this.current)new TeamSettingsDialog({request:this.request,onChange:team=>{this.onChange&&this.onChange(team);},onCompactLead:this.onCompactLead,onResetLead:this.onResetLead}).open(this.current);}
  accepted(){this.refreshRun(true);this.refreshQueue();globalThis.BridgeTeamCanvas?.accepted();}
  schedule(){
    clearTimeout(this.timer);if(!this.current)return;
    // 队列模式下终局 Run 也要继续追队列（新输入排队/停靠），不因 Run 终态停泵。
    if(this.run&&TEAM_TERMINAL_RUN_STATUSES.has(this.run.status)&&!this.queueMode())return;
    this.timer=setTimeout(()=>{this.refreshRun(true);this.refreshQueue();},4000);this.timer.unref?.();
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
      const excluded=this.excludedRuns.get(team.id);
      const rows=(listed.runs||[]).filter(run=>!excluded?.has(run.id));
      const latest=[...rows].reverse().find(run=>!TEAM_TERMINAL_RUN_STATUSES.has(run.status))||rows[rows.length-1];
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
    this.renderQueuePanel();
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
    /* 闭环回音（team-dashboard-design §3 聊天侧·凡事有交代/事事有回音）：
       受理（谁接的单）、最近回音（做到哪）、下一步（在等什么/等谁）。
       与 Dashboard 消费同一服务端投影（teams.go teamRunSummary/detail 的
       currentOwner/lastUpdate/nextAction），两处不给出不同结论；字段缺席
       就不显示，不编造。 */
    const echo=this.node('div',undefined,'team-workflow-echo');
    const leadRow=(team.members||[]).find(m=>m.id===team.leadId);
    const receipt=this.node('div',undefined,'team-workflow-echo-row');
    receipt.append(this.node('span',t_safe('已受理 · Run ')+String(run.id||'').slice(0,8)+(leadRow?' · '+t_safe('负责人 ')+teamWorkflowMemberLabel(leadRow):''),'team-workflow-echo-main'));
    echo.append(receipt);
    if(run.lastUpdate&&run.lastUpdate.summary){
      const row=this.node('div',undefined,'team-workflow-echo-row');
      const at=typeof teamTimeText==='function'?teamTimeText(run.lastUpdate.at):'';
      row.append(this.node('span',t_safe(run.lastUpdate.summary),'team-workflow-echo-main'));
      if(at)row.append(this.node('small',at,'team-workflow-echo-time'));
      echo.append(row);
    }
    const next=run.nextAction;
    if(next&&next.label){
      const row=this.node('div',undefined,'team-workflow-echo-row');
      const chip=this.node('span',t_safe(next.label),'team-workflow-next');
      chip.dataset.kind=next.kind||'';
      row.append(chip);
      // 等待类回音把原因放在触手可及的位置：等待输入/失败/未知必须说清
      // 等谁或出了什么问题，不解释的停靠最伤信任。
      if(next.reason){
        chip.title=next.reason;
        row.append(this.node('small',next.reason,'team-workflow-next-reason'));
      }
      if(next.kind==='answer'||next.kind==='inspect'||next.kind==='resume'){
        const go=this.button(t_safe('去处理'),()=>this.openRun&&this.openRun(team.id,run.id));
        go.className='team-workflow-next-go';
        row.append(go);
      }
      echo.append(row);
    }
    card.append(echo);
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
      for(const task of stageTasks){
        const member=members.get(task.memberId)||{},status=task.status||'planned';
        const failed=status==='failed'||status==='outcome_unknown';
        const station=this.node('div',undefined,'team-workflow-station');
        station.dataset.status=status;
        station.title=String(task.instructionsHead||task.prompt||'').trim();
        station.setAttribute('aria-label',teamWorkflowTaskTitle(task)+' · '+t_safe(teamTaskStatusLabel(status)));
        const nodeHead=this.node('div',undefined,'team-workflow-node-head');
        nodeHead.append(this.node('strong',teamWorkflowTaskTitle(task),'team-workflow-step'));
        const badge=typeof teamStatusBadge==='function'?teamStatusBadge(status,TEAM_TASK_STATUS_LABELS):this.node('span',t_safe(teamTaskStatusLabel(status)),'team-badge b-'+status);
        nodeHead.append(badge);
        station.append(nodeHead);
        const who=this.node('span',undefined,'team-workflow-member');
        who.append(this.node('span',teamWorkflowMemberLabel(member,task),'team-workflow-member-label team-graph-kind'));
        station.append(who);
        // 失败/停靠的站点直接说出原因（Run 详情里也是这句），省掉一次跳转；全文放 title。
        const outcome=status==='stopped'&&TEAM_WORKFLOW_OUTCOME_LABELS[task.outcome];
        const reason=failed?t_safe(String(task.error||'').trim()):outcome?t_safe(outcome):'';
        if(reason){const line=this.node('span',reason,'team-workflow-reason');line.title=reason;station.append(line);}
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

function teamSettingsMemberLabel(member){
  return teamMemberAgentLabel(member,true);
}
function teamReviewerMemberID(team){
  if(!team)return '';
  if(team.reviewerId)return team.reviewerId;
  return (team.members||[]).find(member=>member.enabled&&member.role==='reviewer')?.id||'';
}
class TeamSettingsDialog{
  constructor({request,onChange,onCompactLead,onResetLead}){this.request=request;this.onChange=onChange;this.onCompactLead=onCompactLead;this.onResetLead=onResetLead;this._dialog=null;this.team=null;this.agents=[];this.busy=false;this.error='';this.draftTags=new Map();this.draftText=new Map();}
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
    const dialog=this.dialog();this.team=team;this.error='';this.draftTags.clear();this.draftText.clear();this.render();dialog.showModal();
    try{
      const detail=await this.request('/api/teams/'+encodeURIComponent(team.id));this.team=detail;
      if(detail.project){
        try{const value=await this.request('/api/projects');this.agents=(value.agents||[]).filter(agent=>agent&&agent.kind&&agent.kind!=='codex');}catch{this.agents=[];}
      }
    }catch(error){this.error=error.message;}
    this.render();
  }
  render(){
    if(!this.body)return;
    this.body.replaceChildren();
    if(!this.team){this.body.append(this.node('p',t_safe('正在读取团队…'),'muted'));return;}
    const team=this.team,members=team.members||[],reviewerID=teamReviewerMemberID(team);
    const roles=this.node('section',undefined,'team-settings-section team-role-grid');
    const leadField=this.node('label',undefined,'team-role-card');
    leadField.append(this.node('span',t_safe('主控 Agent'),'team-role-label'));
    const leadSelect=document.createElement('select');leadSelect.className='team-role-select';leadSelect.disabled=this.busy;
    for(const member of members)leadSelect.append(new Option(teamSettingsMemberLabel(member),member.id));
    leadSelect.value=team.leadId||'';leadSelect.onchange=()=>this.changeLead(leadSelect.value);
    leadField.append(leadSelect);
    const reviewerField=this.node('label',undefined,'team-role-card');
    reviewerField.append(this.node('span',t_safe('默认评审 Agent'),'team-role-label'));
    const reviewerSelect=document.createElement('select');reviewerSelect.className='team-role-select';reviewerSelect.disabled=this.busy;
    reviewerSelect.append(new Option(t_safe('不设置默认评审'),''));
    for(const member of members)reviewerSelect.append(new Option(teamSettingsMemberLabel(member),member.id));
    reviewerSelect.value=reviewerID;
    reviewerSelect.onchange=()=>this.changeReviewer(reviewerSelect.value);
    reviewerField.append(reviewerSelect);roles.append(leadField,reviewerField);this.body.append(roles);
    const context=this.node('section',undefined,'team-settings-section team-context-section');
    context.append(this.node('h3',t_safe('主控上下文')));
    const actions=this.node('div',undefined,'team-context-actions');
    const compact=this.button(t_safe('压缩主控上下文'),()=>this.onCompactLead?.(team,()=>this.dialog().close()));
    compact.className='team-context-compact';compact.disabled=this.busy;
    const reset=this.button(t_safe('重置主控会话'),()=>this.onResetLead?.(team,()=>this.dialog().close()));
    reset.className='team-context-reset';reset.disabled=this.busy;
    actions.append(compact,reset);context.append(actions);this.body.append(context);
    const memberSection=this.node('section',undefined,'team-settings-section');
    memberSection.append(this.node('h3',t_safe('成员')));
    memberSection.append(this.node('p',t_safe('空格、逗号或回车生成胶囊；多词标签用双引号。点 × 移除，保存后生效；主控制定工作流时会参考。'),'muted team-settings-tags-hint'));
    for(const member of members){
      const row=this.node('div',undefined,'team-settings-member');
      const info=this.node('div',undefined,'team-member-info');
      info.append(this.node('strong',teamSettingsMemberLabel(member)));
      const roleLabels=[member.id===team.leadId?t_safe('主控'):t_safe('成员')];
      if(member.id===reviewerID)roleLabels.push(t_safe('评审'));
      info.append(this.node('span',roleLabels.join(' · ')+(member.available===false?' · '+t_safe('不可用'):''),'team-member-role'));
      // 能力标签：胶囊内 × 只改草稿，「保存标签」统一落库，
      // 避免误触立即改写主控看到的成员目录。
      const draftTags=()=>this.draftTags.has(member.id)?this.draftTags.get(member.id):(member.tags||[]).slice();
      const tagsField=this.node('div',undefined,'team-member-tags');
      const tagsPlaceholder=t_safe('如：前端 React "Claude Sonnet"');
      const tagsInput=document.createElement('input');tagsInput.value=this.draftText.get(member.id)||'';tagsInput.maxLength=400;
      tagsInput.placeholder=tagsPlaceholder;tagsInput.setAttribute('aria-label',teamSettingsMemberLabel(member)+t_safe(' 的能力标签'));tagsInput.disabled=this.busy;
      const syncSave=()=>{
        const draft=draftTags();
        const pending=this.parseDraftTags(tagsInput.value,true).tags.filter(tag=>!draft.includes(tag));
        tagsInput.placeholder=draft.length?'':tagsPlaceholder;
        saveTags.disabled=this.busy||!this.tagsUnsaved(member,[...draft,...pending]);
      };
      const chipFor=tag=>{
        const chip=this.node('span',undefined,'team-tag-chip');
        chip.append(this.node('span',tag,'team-tag-chip-text'));
        const remove=this.button('×',()=>{this.draftTags.set(member.id,draftTags().filter(item=>item!==tag));chip.remove();syncSave();});
        remove.className='team-tag-chip-remove';remove.disabled=this.busy;
        remove.setAttribute('aria-label',t_safe('移除标签')+'：'+tag);
        chip.append(remove);return chip;
      };
      const addTags=tags=>{
        const additions=tags.filter(tag=>!draftTags().includes(tag));
        if(!additions.length)return;
        this.draftTags.set(member.id,[...draftTags(),...additions]);
        for(const tag of additions)tagsField.insertBefore(chipFor(tag),tagsInput);
        syncSave();
      };
      const saveTags=this.button(t_safe('保存标签'),async()=>{
        const pending=tagsInput.value; // 保存时把未成片的尾段一并收进草稿
        if(pending){tagsInput.value='';this.draftText.delete(member.id);addTags(this.parseDraftTags(pending,true).tags);}
        await this.changeTags(member.id,draftTags());
        // 保存成功回到服务端真值；失败保留草稿供重试。
        if(!this.error){this.draftTags.delete(member.id);this.render();}
      });
      saveTags.className='team-member-save-tags';syncSave();
      const processInput=()=>{
        const {tags,remainder}=this.parseDraftTags(tagsInput.value);
        tagsInput.value=remainder;this.draftText.set(member.id,remainder);
        if(tags.length)addTags(tags);
        syncSave();
      };
      tagsInput.oninput=event=>{
        if(event?.isComposing){this.draftText.set(member.id,tagsInput.value);return;}
        processInput();
      };
      tagsInput.oncompositionend=processInput;
      tagsInput.onkeydown=event=>{
        if(event.key!=='Enter'||event.isComposing||event.keyCode===229)return;
        event.preventDefault();const value=tagsInput.value;tagsInput.value='';this.draftText.set(member.id,'');
        addTags(this.parseDraftTags(value,true).tags);syncSave();
      };
      for(const tag of draftTags())tagsField.append(chipFor(tag));
      tagsField.append(tagsInput,saveTags);info.append(tagsField);row.append(info);
      if(member.id!==team.leadId){const remove=this.button(t_safe('移除'),()=>this.removeMember(member.id));remove.disabled=this.busy;remove.className='team-member-remove';row.append(remove);}
      memberSection.append(row);
    }
    // 同一 Agent 允许出现多名成员（主控兼任兜底成员）：只按成员 id/会话去重，
    // 不按 agent kind 去重。
    const candidates=this.agents;
    const select=document.createElement('select');select.className='team-add-agent';select.disabled=this.busy;
    for(const agent of candidates)select.append(new Option(agent.label||agent.kind,agent.kind));
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
    const current=teamReviewerMemberID(this.team);
    if(memberID===current)return;
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id),{method:'PATCH',body:{reviewerId:memberID,version:this.team.version}}));
  }
  async changeTags(memberID,tags){
    const member=(this.team.members||[]).find(item=>item.id===memberID);if(!member)return;
    const normalized=[...new Set(tags.map(tag=>tag.trim()).filter(Boolean))];
    if(JSON.stringify(normalized)===JSON.stringify(member.tags||[]))return;
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id)+'/members/'+encodeURIComponent(memberID),{method:'PATCH',body:{tags:normalized,version:this.team.version}}));
  }
  parseDraftTags(value,commitTail=false){
    const source=String(value||''),tags=[];
    let token='',quote='',start=0;
    for(let index=0;index<source.length;index++){
      const char=source[index];
      if(quote){if(char===quote)quote='';else token+=char;}
      else if((char==='"'||char==='“')&&!token)quote=char==='"'?'"':'”';
      else if(/[\s,，]/u.test(char)){
        if(token.trim())tags.push(token.trim());
        token='';start=index+1;
      }else token+=char;
    }
    if(commitTail&&token.trim())tags.push(token.trim());
    return{tags:[...new Set(tags)],remainder:source.slice(start).trimStart()};
  }
  tagsUnsaved(member,draft){return JSON.stringify(draft)!==JSON.stringify(member.tags||[]);}
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
    try{this.team=await operation();this.onChange&&this.onChange(this.team);globalThis.BridgeFeedback?.ok('团队设置已更新');}
    catch(error){this.error=error.message;}
    finally{this.busy=false;this.render();}
  }
}
if(typeof module!=='undefined')module.exports={splitTeamActions,renderTeamActionsCard,renderTeamDeliveryEvidence,TeamChatController,TeamSettingsDialog,TeamLeadPicker,teamSubmissionFields,teamQueueModeEnabled,teamInputStatusLabel,teamInputHead,teamWorkflowStages,teamWorkflowTaskTitle,teamWorkflowCounts,teamWorkflowOutcome,teamWorkflowMemberLabel};
