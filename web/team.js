'use strict';
/* Team 编排前端（阶段一 + 阶段二）：TeamsPanel 负责团队列表、新建/重命名/删除与
   Run 创建入口（照 PoolPanel 的 loading/error/render 全量重建模式）；TeamRunScreen
   挂在 #main 的 #team-run 区块内展示单个 Run 的任务卡与 salvage 结果，事件经
   BridgeTransport.subscribe 增量上屏（changes 长轮询 / SSE 同一套回调）。
   阶段二增量：broadcast 创建、成员转发、worktree diff 审查（复用 BridgeDiff，
   只读、不改 diff.js）、验证闸门徽标；meta 消费面扩展 mode/verify 摘要。
   依赖 app.js 的全局 csrf / showLogin（team.js 先于 app.js 加载，仅运行时引用）。 */

const TEAM_ROLE_LABELS={lead:'负责人',member:'成员',reviewer:'评审'};
/* 任务停靠分类（task_stopped/task_failed payload 的 outcome，§6.3 错误策略表）：
   quota/auth/not_configured 类停靠落 run waiting_input，处理后 resume 重派。 */
const TEAM_TASK_OUTCOME_LABELS={rate_limited:'触发限流',quota:'额度不足',auth:'成员认证失败',not_configured:'成员未配置',context_overflow:'上下文超限',transient:'临时错误'};
/* broadcast 收集策略（§6.5）：all 保留每个分支结果；first_success 首个成功后取消其余。 */
const TEAM_COLLECT_OPTIONS=[
  {key:'all',label:'全部结果（保留每个分支的结果）'},
  {key:'first_success',label:'首个成功即止（成功后取消其余分支）'}];
/* 转发会话转录的范围（与迁移转录同口径：migrate.go main/full/latest）。 */
const TEAM_FORWARD_SCOPES=[
  {key:'main',label:'主要历史（标题与最近消息）'},
  {key:'full',label:'完整历史'},
  {key:'latest',label:'最新一轮提问与结果'}];
/* 客户端幂等键（Run 创建与成员转发）：crypto.randomUUID 不可用时降级；
   app.js 的 uuid() 属其模块内部，这里独立实现避免加载顺序耦合。 */
function teamUUID(){
  if(typeof crypto!=='undefined'&&typeof crypto.randomUUID==='function')return crypto.randomUUID();
  return Date.now().toString(16)+'-'+Math.random().toString(16).slice(2)+'-'+Math.random().toString(16).slice(2,10);
}
const TEAM_RUN_STATUS_LABELS={created:'已创建',dispatching:'派发中',collecting:'收集中',reducing:'汇总中',completed:'已完成',waiting_input:'等待确认',failed:'已失败',cancelled:'已取消',stopped:'已停止',outcome_unknown:'结果未知'};
const TEAM_TASK_STATUS_LABELS={planned:'已规划',ready:'就绪',dispatching:'派发中',running:'运行中',completed:'已完成',failed:'已失败',cancelled:'已取消',outcome_unknown:'结果未知',stopped:'已停止'};
const TEAM_TERMINAL_RUN_STATUSES=new Set(['completed','failed','cancelled','stopped','outcome_unknown']);
/* 事件类型 → 任务状态（服务端事件行不携带 status 字段，状态由 type 推导；
   task_stopped 在用户取消 Run 时也用于 cancelled 任务——cancel 后的整页
   重读会立即校正）。 */
const TEAM_EVENT_TASK_STATUS={task_dispatching:'dispatching',task_running:'running',task_completed:'completed',task_failed:'failed',task_stopped:'stopped',task_outcome_unknown:'outcome_unknown',task_retry_scheduled:'ready'};

function teamRunStatusLabel(status){return TEAM_RUN_STATUS_LABELS[status]||status;}
function teamTaskStatusLabel(status){return TEAM_TASK_STATUS_LABELS[status]||status;}
// 状态徽标：label 同时写进 data-i18n，切语言时 BridgeI18n.apply 可热刷新。
function teamStatusBadge(status,labels){
  const label=(labels&&labels[status])||status;
  const node=document.createElement('span');
  node.className='team-badge b-'+status;
  node.textContent=BridgeI18n.t(label);
  node.setAttribute('data-i18n',label);
  return node;
}
function teamTimeText(value){
  if(!value)return '';
  const date=new Date(value);
  return isNaN(date.getTime())?String(value):date.toLocaleString(BridgeI18n.locale());
}
function t_safe(text){return typeof BridgeI18n!=='undefined'&&BridgeI18n.t?BridgeI18n.t(text):text;}
/* 与 app.js 的 api() 同语义，但支持 PATCH/DELETE（Team 契约使用真实 HTTP 方法）：
   GET 无 body；其余方法发 JSON body（缺省 {}）并携带 X-CSRF-Token。 */
async function teamRequest(path,{method='GET',body}={}){
  const options={credentials:'same-origin',cache:'no-store',headers:{}};
  if(method==='GET')options.method='GET';
  else{
    options.method=method;
    options.headers={'Content-Type':'application/json','X-CSRF-Token':typeof csrf==='string'?csrf:''};
    options.body=JSON.stringify(body===undefined?{}:body);
  }
  const response=await fetch(path,options);
  const data=await response.json().catch(()=>({}));
  if(!response.ok){
    if(response.status===401&&typeof showLogin==='function')showLogin();
    throw Error(data.error||(typeof t==='function'?t('请求失败'):'请求失败'));
  }
  return data;
}

class TeamsPanel{
  constructor({root,request,openRun}){
    Object.assign(this,{root,request,openRun});
    this.teams=[];this.loading=false;this.busy=false;this.error='';this.notice='';
    this.create=null;       // 新建团队表单状态
    this.renaming=null;     // 重命名中的 teamId
    this.renameValue='';
    this.expanded=new Set(); // Run 列表展开的 teamId
    this.runs=new Map();    // teamId -> {loading,error,rows}
    this.sessions=null;     // 新建表单可用的 ACP 会话（打开面板时快照）
  }
  node(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
  button(text,fn){const n=this.node('button',text);n.type='button';n.onclick=fn;return n;}
  runBadge(status){return teamStatusBadge(status,TEAM_RUN_STATUS_LABELS);}
  async refresh(){
    if(this.loading)return;
    this.loading=true;this.error='';this.render();
    try{const value=await this.request('/api/teams');this.teams=value.teams||[];}
    catch(error){this.error=error.message;}
    finally{this.loading=false;this.render();}
  }
  async send(method,path,body){
    if(this.busy)return null;
    this.busy=true;this.error='';this.render();
    try{return await this.request(path,{method,body});}
    catch(error){this.error=error.message;return null;}
    finally{this.busy=false;this.render();}
  }
  // 变更成功后先刷新列表，再贴提示：提示要在最终渲染里存活，不能被 loading 中间帧消费掉。
  async notify(message){
    await this.refresh();
    this.notice=message;
    this.render();
  }
  async loadRuns(teamId){
    if(this.busy)return;
    const entry={loading:true,error:'',rows:this.runs.get(teamId)?.rows||[]};
    this.runs.set(teamId,entry);
    this.render();
    try{const result=await this.request('/api/teams/'+encodeURIComponent(teamId)+'/runs');entry.rows=result.runs||[];}
    catch(error){entry.error=error.message;}
    finally{entry.loading=false;this.render();}
  }
  render(){
    const t=BridgeI18n.t,root=this.root;
    root.replaceChildren();
    const toolbar=this.node('div',undefined,'team-toolbar');
    const refresh=this.button(t('刷新'),()=>this.refresh());
    refresh.disabled=this.loading||this.busy;
    const create=this.button(this.create?t('收起新建'):t('新建团队'),()=>{
      this.create=this.create?null:{name:'',error:'',members:[{sessionId:'',role:'lead',name:''}]};
      if(this.create)this.loadSessions();
      this.render();
    });
    create.disabled=this.busy;
    toolbar.append(refresh,create);
    root.append(toolbar);
    if(this.error)root.append(this.node('p',this.error,'error'));
    if(this.notice){root.append(this.node('p',this.notice,'team-notice'));this.notice='';}
    if(this.loading&&!this.teams.length){root.append(this.node('p',t('正在读取团队…'),'muted'));return;}
    if(this.create)root.append(this.renderCreateForm());
    if(!this.teams.length){
      if(!this.create)root.append(this.node('p',t('还没有团队。点击「新建团队」，把已有的 ACP 会话绑定成成员。'),'muted'));
      return;
    }
    for(const team of this.teams)root.append(this.renderTeam(team));
  }
  renderTeam(team){
    const t=BridgeI18n.t;
    const card=this.node('section',undefined,'team-card');
    card.dataset.id=team.id;
    const head=this.node('div',undefined,'team-card-head');
    if(this.renaming===team.id){
      const input=document.createElement('input');
      input.value=this.renameValue;input.maxLength=120;
      input.oninput=()=>{this.renameValue=input.value;};
      const save=this.button(t('保存'),async()=>{
        const name=this.renameValue.trim();
        if(!name)return;
        const result=await this.send('PATCH','/api/teams/'+encodeURIComponent(team.id),{name,version:team.version});
        if(result){this.renaming=null;await this.notify(t('团队已重命名。'));}
      });
      const cancel=this.button(t('取消'),()=>{this.renaming=null;this.render();});
      head.append(input,save,cancel);
    }else head.append(this.node('h3',team.name));
    card.append(head);
    const members=team.members||[];
    const meta=this.node('p',undefined,'muted team-card-meta');
    const parts=[members.length+' '+t('名成员')];
    const updated=teamTimeText(team.updatedAt);
    if(updated)parts.push(t('更新于 ')+updated);
    meta.textContent=parts.join(' · ');
    card.append(meta);
    const chips=this.node('div',undefined,'team-members');
    for(const m of members){
      const chip=this.node('span',undefined,'team-member'+(m.available===false?' unavailable':'')+(m.enabled===false?' disabled':''));
      const role=(TEAM_ROLE_LABELS[m.role]||m.role||'member');
      chip.textContent=(m.id===team.leadId?'★ ':'')+role+' · '+(m.name||m.id)+' · '+BridgeI18n.agentLabel(m.agentKind)
        +(m.available===false?' · '+t('不可用'):'')+(m.enabled===false?' · '+t('已停用'):'');
      chip.title=t('会话：')+(m.sessionId||'');
      chips.append(chip);
    }
    if(members.length)card.append(chips);
    const policy=team.policy||{};
    card.append(this.node('p',t('最大并行 ')+(policy.maxParallelTasks??'-')+' · '+t('最大轮次 ')+(policy.maxRounds??'-')+' · '+t('自动派发 ')+(policy.autoDispatch?t('开'):t('关')),'muted team-policy'));
    const actions=this.node('div',undefined,'team-actions');
    const runsBtn=this.button(this.expanded.has(team.id)?t('收起 Run'):t('Run 记录'),()=>{
      if(this.expanded.has(team.id))this.expanded.delete(team.id);
      else{this.expanded.add(team.id);this.loadRuns(team.id);}
      this.render();
    });
    const createRun=this.button(t('新建 Run'),()=>this.openRunCreate(team));
    actions.append(runsBtn,createRun);
    if(this.renaming!==team.id){
      actions.append(this.button(t('重命名'),()=>{this.renaming=team.id;this.renameValue=team.name;this.render();}));
    }
    actions.append(this.button(t('删除'),async()=>{
      if(!window.confirm(t('删除团队会移除它的 Run 记录；仍有未结束的 Run 时会被拒绝。确认删除？')+'\n'+team.name))return;
      const result=await this.send('DELETE','/api/teams/'+encodeURIComponent(team.id));
      if(result)await this.notify(t('团队已删除。'));
    }));
    if(this.busy)actions.querySelectorAll('button').forEach(b=>b.disabled=true);
    card.append(actions);
    if(this.expanded.has(team.id))card.append(this.renderRuns(team));
    return card;
  }
  renderRuns(team){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'team-runs');
    const entry=this.runs.get(team.id);
    if(!entry||entry.loading){box.append(this.node('p',t('正在读取 Run…'),'muted'));return box;}
    if(entry.error){box.append(this.node('p',entry.error,'error'));return box;}
    if(!entry.rows.length){box.append(this.node('p',t('还没有 Run。'),'muted'));return box;}
    for(const run of entry.rows){
      const row=this.button('');
      row.className='team-run-row';
      row.append(this.runBadge(run.status));
      row.append(this.node('span',run.goal||run.id,'team-run-goal'));
      const updated=teamTimeText(run.updatedAt);
      if(updated)row.append(this.node('small',updated,'muted'));
      if(run.stopReason)row.title=teamRunStatusLabel(run.status)+' · '+run.stopReason;
      row.onclick=()=>this.openRun&&this.openRun(team.id,run.id);
      box.append(row);
    }
    return box;
  }
  // 新建表单的会话下拉只列本机 ACP 会话（阶段一 Team 成员仅支持 ACP Agent 会话）。
  async loadSessions(){
    if(this.sessions||this._sessionsLoading)return;
    this._sessionsLoading=true;
    try{
      const result=await this.request('/api/sessions?q=&offset=0');
      this.sessions=(result.sessions||[]).filter(row=>row.host==='local'&&row.agent&&row.agent!=='codex');
    }catch{this.sessions=[];}
    finally{
      this._sessionsLoading=false;
      if(this.create)this.render();
    }
  }
  sessionLabel(row){
    const bits=[row.title||row.id];
    if(row.projectName)bits.push(row.projectName);
    bits.push(BridgeI18n.agentLabel(row.agent));
    return bits.join(' · ');
  }
  renderCreateForm(){
    const t=BridgeI18n.t,create=this.create;
    const form=this.node('form',undefined,'team-create');
    form.append(this.node('h3',t('新建团队')));
    const nameLabel=this.node('label',undefined,'');
    nameLabel.append(this.node('span',t('团队名称')));
    const nameInput=document.createElement('input');
    nameInput.value=create.name;nameInput.maxLength=120;
    nameInput.oninput=()=>{create.name=nameInput.value;};
    nameLabel.append(nameInput);
    form.append(nameLabel);
    const rowsBox=this.node('div',undefined,'team-member-rows');
    const sessions=this.sessions;
    for(const [index,row] of create.members.entries()){
      const box=this.node('div',undefined,'member-row');
      const memberLabel=this.node('label');
      memberLabel.append(this.node('span',t('成员会话')));
      const select=document.createElement('select');
      if(!sessions){
        select.disabled=true;
        select.append(new Option(t('正在读取会话…'),''));
      }else if(!sessions.length){
        select.disabled=true;
        select.append(new Option(t('没有可用的 ACP 会话'),''));
      }else{
        select.append(new Option(t('请选择会话…'),''));
        for(const session of sessions)select.append(new Option(this.sessionLabel(session),session.id));
        select.value=row.sessionId||'';
      }
      select.onchange=()=>{row.sessionId=select.value;};
      memberLabel.append(select);
      box.append(memberLabel);
      const roleLabel=this.node('label');
      roleLabel.append(this.node('span',t('角色')));
      const roleSelect=document.createElement('select');
      for(const [value,label] of Object.entries(TEAM_ROLE_LABELS))roleSelect.append(new Option(t(label),value));
      roleSelect.value=row.role;
      roleSelect.onchange=()=>{row.role=roleSelect.value;};
      roleLabel.append(roleSelect);
      box.append(roleLabel);
      const nameLabel2=this.node('label');
      nameLabel2.append(this.node('span',t('备注名（可选）')));
      const nameField=document.createElement('input');
      nameField.value=row.name||'';nameField.maxLength=120;
      nameField.oninput=()=>{row.name=nameField.value;};
      nameLabel2.append(nameField);
      box.append(nameLabel2);
      if(create.members.length>1){
        const remove=this.button(t('移除'),()=>{create.members.splice(index,1);this.render();});
        box.append(remove);
      }
      rowsBox.append(box);
    }
    form.append(rowsBox);
    form.append(this.button(t('＋ 添加成员'),()=>{
      create.members.push({sessionId:'',role:'member',name:''});
      this.render();
    }));
    if(create.error)form.append(this.node('p',create.error,'error'));
    const submit=document.createElement('button');
    submit.type='submit';submit.className='primary';submit.textContent=t('创建团队');
    submit.disabled=this.busy||!sessions||!sessions.length;
    form.append(submit);
    form.onsubmit=async event=>{
      event.preventDefault();
      if(this.busy)return;
      create.error='';
      const name=create.name.trim();
      if(!name){create.error=t('请填写团队名称。');this.render();return;}
      const members=[];
      for(const [i,item] of create.members.entries()){
        if(!item.sessionId){create.error=t('请为每位成员选择一个会话。');this.render();return;}
        const member={id:'member-'+(i+1),sessionId:item.sessionId,role:item.role};
        if(item.name.trim())member.name=item.name.trim();
        members.push(member);
      }
      const lead=members.find(m=>m.role==='lead')||members[0];
      const team=await this.send('POST','/api/teams',{name,members,leadId:lead.id});
      if(team){
        this.create=null;
        await this.notify(team.duplicate?t('团队已存在（内容一致），未重复创建。'):t('团队已创建。'));
      }
    };
    return form;
  }
  /* Run 创建对话框（<dialog class="picker"> 动态创建，照 openQueueEditor 惯例）：
     第一屏填 goal/mode/任务行，第二屏按提交顺序+依赖做确认预览（§11 默认保留预览）。 */
  runDialog(){
    if(this._runDialog)return this._runDialog;
    const dialog=document.createElement('dialog');
    dialog.id='team-run-dialog';dialog.className='picker';
    dialog.setAttribute('aria-labelledby','team-run-dialog-heading');
    const head=document.createElement('div');head.className='picker-head';
    const heading=document.createElement('h2');heading.id='team-run-dialog-heading';
    const close=document.createElement('button');
    close.type='button';close.className='icon-button';close.textContent='×';
    close.setAttribute('aria-label',t_safe('关闭'));
    close.setAttribute('data-i18n-aria-label','关闭');
    close.onclick=()=>{dialog.runState=null;dialog.close();};
    head.append(heading,close);
    const body=document.createElement('div');body.className='picker-body';
    dialog.append(head,body);
    document.body.append(dialog);
    this._runDialog=dialog;
    return dialog;
  }
  openRunCreate(team){
    const dialog=this.runDialog();
    dialog.runState={
      team,phase:'edit',busy:false,error:'',
      goal:'',mode:'single',collect:'all',
      tasks:[{id:'t1',memberId:'',memberIds:new Set(),deps:new Set(),prompt:'',writeLock:'none'}],
      parsed:[],requestId:null,
    };
    this.renderRunDialog();
    dialog.showModal();
  }
  /* 计划行 → 提交 plan（single 模式不用）：pipeline 单成员（memberId），
     broadcast 多成员（memberIds ≥2，服务端按 BroadcastGroup 展开为逐成员任务）。 */
  parseRunTasks(state){
    const t=BridgeI18n.t;
    const broadcast=state.mode==='broadcast';
    const ids=new Set();
    for(const row of state.tasks){
      const id=row.id.trim();
      if(!id){state.error=t('请为每条任务填写任务 ID。');return null;}
      if(ids.has(id)){state.error=t('任务 ID 重复：')+id;return null;}
      ids.add(id);
    }
    const tasks=[];
    for(const row of state.tasks){
      if(broadcast){
        if(row.memberIds.size<2){state.error=t('并行分派的每条任务至少选择 2 名成员。');return null;}
      }else if(!row.memberId){
        state.error=t('请为每条任务选择成员。');return null;
      }
      if(!row.prompt.trim()){state.error=t('请为每条任务填写提示词。');return null;}
      const dependsOn=[...row.deps].filter(dep=>dep!==row.id.trim()&&ids.has(dep));
      const task={id:row.id.trim(),prompt:row.prompt,writeLock:row.writeLock};
      if(broadcast)task.memberIds=[...row.memberIds];
      else task.memberId=row.memberId;
      if(dependsOn.length)task.dependsOn=dependsOn;
      tasks.push(task);
    }
    return tasks;
  }
  renderRunDialog(){
    const dialog=this._runDialog,state=dialog&&dialog.runState;
    if(!state)return;
    const t=BridgeI18n.t;
    dialog.querySelector('h2').textContent=state.phase==='preview'?t('确认 Run 计划'):t('新建 Run')+' · '+state.team.name;
    const body=dialog.querySelector('.picker-body');
    body.replaceChildren();
    const form=document.createElement('form');
    if(state.phase==='edit'){
      const goalLabel=this.node('label');
      goalLabel.append(this.node('span',t('目标')));
      const goal=document.createElement('textarea');
      goal.rows=3;goal.maxLength=10000;
      goal.value=state.goal;
      goal.oninput=()=>{state.goal=goal.value;};
      goalLabel.append(goal);
      form.append(goalLabel);
      const modeLabel=this.node('label');
      modeLabel.append(this.node('span',t('运行模式')));
      const mode=document.createElement('select');
      mode.append(new Option(t('单任务（自动合成负责人任务）'),'single'));
      mode.append(new Option(t('流水线（自定义任务与依赖）'),'pipeline'));
      mode.append(new Option(t('并行分派（同一任务发多名成员）'),'broadcast'));
      mode.value=state.mode;
      mode.onchange=()=>{state.mode=mode.value;this.renderRunDialog();};
      modeLabel.append(mode);
      form.append(modeLabel);
      if(state.mode==='broadcast'){
        const collectLabel=this.node('label');
        collectLabel.append(this.node('span',t('收集策略')));
        const collect=document.createElement('select');
        for(const option of TEAM_COLLECT_OPTIONS)collect.append(new Option(t(option.label),option.key));
        collect.value=state.collect;
        collect.onchange=()=>{state.collect=collect.value;};
        collectLabel.append(collect);
        form.append(collectLabel);
      }
      if(state.mode!=='single'){
        const broadcast=state.mode==='broadcast';
        const rowsBox=this.node('div',undefined,'team-member-rows');
        for(const [index,row] of state.tasks.entries()){
          const box=this.node('div',undefined,'member-row team-task-row');
          const head=this.node('div',undefined,'member-row-head');
          const idLabel=this.node('label');
          idLabel.append(this.node('span',t('任务 ID')));
          const idInput=document.createElement('input');
          idInput.value=row.id;idInput.maxLength=64;
          idInput.oninput=()=>{row.id=idInput.value;};
          idLabel.append(idInput);
          head.append(idLabel);
          if(!broadcast){
            const memberLabel=this.node('label');
            memberLabel.append(this.node('span',t('成员')));
            const memberSelect=document.createElement('select');
            const members=(state.team.members||[]).filter(m=>m.enabled!==false);
            memberSelect.append(new Option(t('请选择成员…'),''));
            for(const member of members)memberSelect.append(new Option((member.id===state.team.leadId?t('负责')+' · ':'')+(member.name||member.id)+' · '+BridgeI18n.agentLabel(member.agentKind),member.id));
            memberSelect.value=row.memberId||'';
            memberSelect.onchange=()=>{row.memberId=memberSelect.value;};
            memberLabel.append(memberSelect);
            head.append(memberLabel);
          }
          const lockLabel=this.node('label');
          lockLabel.append(this.node('span',t('写锁')));
          const lockSelect=document.createElement('select');
          lockSelect.append(new Option(t('无（只读/无副作用）'),'none'));
          lockSelect.append(new Option(t('独占（持有项目写锁）'),'exclusive'));
          lockSelect.value=row.writeLock;
          lockSelect.onchange=()=>{row.writeLock=lockSelect.value;};
          lockLabel.append(lockSelect);
          head.append(lockLabel);
          if(state.tasks.length>1){
            const remove=this.button(t('移除'),()=>{state.tasks.splice(index,1);for(const other of state.tasks)other.deps.delete(row.id);this.renderRunDialog();});
            head.append(remove);
          }
          box.append(head);
          if(broadcast){
            // 并行分派：成员多选 chip（≥2 校验在 parseRunTasks），放在 head 下方
            // 独立区块（head 是定宽字段行），复用依赖 chip 的样式保持视觉一致。
            const memberLabel=this.node('label');
            memberLabel.append(this.node('span',t('成员（点选 ≥2 名）')));
            const chips=this.node('div',undefined,'task-deps');
            const enabled=(state.team.members||[]).filter(m=>m.enabled!==false);
            if(!enabled.length)chips.append(this.node('span',t('没有可用成员'),'muted'));
            for(const member of enabled){
              const name=(member.id===state.team.leadId?t('负责')+' · ':'')+(member.name||member.id)+' · '+BridgeI18n.agentLabel(member.agentKind);
              const chip=this.button((row.memberIds.has(member.id)?'✓ ':'')+name,'');
              chip.className='task-dep-chip'+(row.memberIds.has(member.id)?' on':'');
              chip.onclick=()=>{
                if(row.memberIds.has(member.id))row.memberIds.delete(member.id);
                else row.memberIds.add(member.id);
                this.renderRunDialog();
              };
              chips.append(chip);
            }
            memberLabel.append(chips);
            box.append(memberLabel);
          }
          const promptLabel=this.node('label');
          promptLabel.append(this.node('span',t('提示词')));
          const prompt=document.createElement('textarea');
          prompt.rows=3;prompt.maxLength=100000;
          prompt.value=row.prompt;
          prompt.oninput=()=>{row.prompt=prompt.value;};
          promptLabel.append(prompt);
          box.append(promptLabel);
          const others=state.tasks.filter(other=>other!==row);
          if(others.length){
            const deps=this.node('div',undefined,'task-deps');
            deps.append(this.node('span',t('依赖：'),'task-deps-label'));
            for(const other of others){
              const otherId=other.id.trim();
              const chip=this.button((row.deps.has(otherId)?'✓ ':'')+otherId,'');
              chip.className='task-dep-chip'+(row.deps.has(otherId)?' on':'');
              chip.disabled=!otherId;
              chip.onclick=()=>{
                if(row.deps.has(otherId))row.deps.delete(otherId);
                else row.deps.add(otherId);
                this.renderRunDialog();
              };
              deps.append(chip);
            }
            box.append(deps);
          }
          rowsBox.append(box);
        }
        form.append(rowsBox);
        form.append(this.button(t('＋ 添加任务'),()=>{
          state.tasks.push({id:'t'+(state.tasks.length+1),memberId:'',memberIds:new Set(),deps:new Set(),prompt:'',writeLock:'none'});
          this.renderRunDialog();
        }));
      }
    }else{
      const preview=this.node('ol',undefined,'team-plan-preview');
      const memberName=memberId=>{
        const member=(state.team.members||[]).find(m=>m.id===memberId);
        return member?(member.name||member.id):memberId;
      };
      if(state.mode==='single'){
        const lead=(state.team.members||[]).find(m=>m.id===state.team.leadId)||((state.team.members||[])[0]||null);
        const item=this.node('li');
        item.append(this.node('strong',t('单任务')+' · '+(lead?(lead.name||lead.id):t('未指定'))));
        item.append(this.node('p',t('将为负责人自动合成一条任务，提示词即目标内容。'),'muted'));
        preview.append(item);
      }else if(state.mode==='broadcast'){
        // 按 BroadcastGroup 分组预览：每条计划任务展开为逐成员并行任务，
        // 同组共用同一段提示词（服务端展开，taskId 粒度按成员细化）。
        for(const [index,task] of state.parsed.entries()){
          const item=this.node('li');
          item.append(this.node('strong',(index+1)+'. '+task.id+' · '+t('并行分派')+' '+task.memberIds.length+' '+t('名成员')));
          const meta=this.node('p',undefined,'muted');
          const bits=[t('写锁：')+(task.writeLock==='exclusive'?t('独占'):t('无'))];
          if(task.dependsOn?.length)bits.push(t('依赖：')+task.dependsOn.join('、'));
          bits.push(t('展开为：')+task.memberIds.map(memberName).join('、'));
          meta.textContent=bits.join(' · ');
          item.append(meta);
          const head=task.prompt.length>80?task.prompt.slice(0,80)+'…':task.prompt;
          item.append(this.node('p',head,'task-instructions'));
          preview.append(item);
        }
        const collect=TEAM_COLLECT_OPTIONS.find(option=>option.key===state.collect);
        const summary=this.node('li');
        summary.append(this.node('p',t('收集策略：')+(collect?t(collect.label):state.collect),'muted'));
        preview.append(summary);
      }else{
        for(const [index,task] of state.parsed.entries()){
          const item=this.node('li');
          item.append(this.node('strong',(index+1)+'. '+task.id+' · '+memberName(task.memberId)));
          const meta=this.node('p',undefined,'muted');
          const bits=[t('写锁：')+(task.writeLock==='exclusive'?t('独占'):t('无'))];
          if(task.dependsOn?.length)bits.push(t('依赖：')+task.dependsOn.join('、'));
          meta.textContent=bits.join(' · ');
          item.append(meta);
          const head=task.prompt.length>80?task.prompt.slice(0,80)+'…':task.prompt;
          item.append(this.node('p',head,'task-instructions'));
          preview.append(item);
        }
      }
      form.append(preview);
      form.append(this.node('p',state.mode==='broadcast'
        ?t('同组任务并行执行，互不等待；收集策略为「首个成功即止」时，首个成功会取消其余分支。创建后可在 Run 页随时取消。')
        :t('任务按以上顺序派发；依赖就绪后才实际发送。创建后可在 Run 页随时取消。'),'muted'));
    }
    if(state.error)form.append(this.node('p',state.error,'error'));
    const actions=this.node('div',undefined,'picker-actions');
    if(state.phase==='preview'){
      const back=this.button(t('返回修改'),()=>{
        // 返回编辑即放弃当前幂等键：内容可能变化，重用同 id 会被「同 id 异内容」拒绝。
        state.requestId=null;
        state.phase='edit';
        this.renderRunDialog();
      });
      back.disabled=state.busy;
      actions.append(back);
    }
    const submit=document.createElement('button');
    submit.type='submit';submit.className='primary';
    submit.textContent=state.phase==='preview'?t('开始运行'):t('预览计划');
    submit.disabled=state.busy;
    actions.append(submit);
    form.append(actions);
    form.onsubmit=async event=>{
      event.preventDefault();
      if(state.busy)return;
      state.error='';
      if(!state.goal.trim()){state.error=t('请填写目标。');this.renderRunDialog();return;}
      if(state.phase==='edit'){
        if(state.mode!=='single'){
          const tasks=this.parseRunTasks(state);
          if(!tasks){this.renderRunDialog();return;}
          state.parsed=tasks;
        }else state.parsed=[];
        state.phase='preview';
        this.renderRunDialog();
        return;
      }
      const payload={goal:state.goal.trim(),mode:state.mode};
      if(state.mode!=='single'){
        payload.plan=state.parsed;
        // 服务端契约读取 collectPolicy（teams.go handleRunCreate 的
        // stringField(body,"collectPolicy")；测试 teams_test.go:407 同键）。
        if(state.mode==='broadcast')payload.collectPolicy=state.collect;
      }
      // 请求幂等键（可选 UUID）：失败后直接再点「开始运行」复用同 id 重放，
      // 不会在网络抖动下重复建 Run；返回编辑则重生成。
      if(!state.requestId)state.requestId=teamUUID();
      payload.id=state.requestId;
      state.busy=true;this.renderRunDialog();
      try{
        const run=await this.request('/api/teams/'+encodeURIComponent(state.team.id)+'/runs',{method:'POST',body:payload});
        dialog.runState=null;
        dialog.close();
        this.openRun&&this.openRun(state.team.id,run.id);
        this.loadRuns(state.team.id);
      }catch(error){
        state.busy=false;state.error=error.message;
        this.renderRunDialog();
      }
    };
    body.append(form);
  }
}

class TeamRunScreen{
  constructor({root,request,openChat,onUnauthorized,onNavigateAway}){
    Object.assign(this,{root,request,openChat,onUnauthorized,onNavigateAway});
    this.teamId=null;this.runId=null;this.team=null;this.detail=null;this.meta=null;
    this.epoch='';this.lastSeq=-1;this.generation=0;
    this.stopFn=null;this.refetchTimer=null;this.actionError='';
    this.terminalSynced=false; // 终态后的整页补读只做一次
    this.refetchFailures=0;    // 补读失败的有限重试计数
    this.reconnecting=false;   // 断线 chip：由 'reconnecting' 置位、成功帧自清
    this._lastMetaJSON='';     // 空帧守卫的 meta 内容比较基准
    this.taskDiffs=new Map();  // taskId -> {loading,error,data}：diff 拉取缓存（帧重渲染不丢展开态）
    this.diffOpen=new Set();   // 已展开「查看改动」的 taskId
    this.actionNotice='';      // 动作成功提示（转发等），随下一次动作/打开清空
    this._forwardDialog=null;
  }
  node(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
  button(text,fn){const n=this.node('button',text);n.type='button';n.onclick=fn;return n;}
  runBadge(status){return teamStatusBadge(status,TEAM_RUN_STATUS_LABELS);}
  taskBadge(status){return teamStatusBadge(status,TEAM_TASK_STATUS_LABELS);}
  teamMember(memberId){
    if(!this.team)return null;
    return (this.team.members||[]).find(m=>m.id===memberId)||null;
  }
  runUrl(action){return '/api/teams/'+encodeURIComponent(this.teamId)+'/runs/'+encodeURIComponent(this.runId)+(action?'/'+action:'');}
  async openRun(teamId,runId){
    this.close();
    const generation=++this.generation;
    this.teamId=teamId;this.runId=runId;
    this.team=null;this.detail=null;this.meta=null;this.actionError='';this.actionNotice='';
    this.epoch='';this.lastSeq=-1;this._lastMetaJSON='';
    this.terminalSynced=false;this.refetchFailures=0;
    this.reconnecting=false;this.taskDiffs.clear();this.diffOpen.clear();
    this.root.replaceChildren();
    const loading=this.node('div',undefined,'team-run-body');
    loading.append(this.node('p',BridgeI18n.t('正在读取 Run…'),'muted'));
    this.root.append(loading);
    try{
      // 团队信息用于成员名/会话映射；读取失败不阻塞 Run 展示。
      const [team,detail]=await Promise.all([
        this.request('/api/teams/'+encodeURIComponent(teamId)).catch(()=>null),
        this.request('/api/teams/'+encodeURIComponent(teamId)+'/runs/'+encodeURIComponent(runId)),
      ]);
      if(generation!==this.generation)return;
      this.team=team;this.detail=detail;
      this.render();
      this.subscribe();
    }catch(error){
      if(generation!==this.generation)return;
      this.root.replaceChildren();
      const body=this.node('div',undefined,'team-run-body');
      body.append(this.node('p',error.message,'error'));
      body.append(this.button(BridgeI18n.t('返回'),()=>this.leave()));
      this.root.append(body);
    }
  }
  subscribe(){
    if(this.stopFn)this.stopFn();
    const generation=this.generation;
    this.stopFn=BridgeTransport.subscribe(
      action=>'/api/teams/'+encodeURIComponent(this.teamId)+'/runs/'+encodeURIComponent(this.runId)+'/'+action,
      {
        onChanges:page=>{if(generation===this.generation)this.applyFrame(page);},
        onReset:page=>{if(generation===this.generation)this.applyFrame(page);},
        onStatus:text=>{
          if(generation!==this.generation)return;
          if(text==='unauthorized'){this.close();this.onUnauthorized&&this.onUnauthorized();return;}
          // transport 只有 reconnecting/unauthorized 两态、无「已恢复」信号
          // （transport.js retry() 每次重试发 reconnecting）；置位断线 chip，
          // 恢复由 applyFrame 入口按「成功帧到达」自清。
          if(text==='reconnecting'&&!this.reconnecting){this.reconnecting=true;this.render();}
        },
        cursor:()=>({after:this.lastSeq,epoch:this.epoch}),
      });
  }
  /* 事件载荷是有限摘要（§10.3）。服务端事件行契约（model.go eventToObject）：
     顶层只有 id/runId/type/source/payload/idempotencyKey?/sequence/createdAt，
     任务 id 在 payload.taskId、游标在 sequence——就地更新任务卡按事件类型
     推导状态，随后补一次完整 RunDetail 读取兜底细节；reset 帧直接全量重读。
     run 级事件（task_forwarded、run_verify、workspace_bound 等无 taskId 事件）
     无 taskId，在 applyEvent 早退后由 400ms 兜底补读整页刷新，不重建任务卡。
     空帧（轮询传输的超时返回、SSE 心跳后的空增量）不重渲染。 */
  applyFrame(page){
    if(!this.detail||!page||typeof page!=='object')return;
    // 成功帧到达即代表通道恢复：在此自清断线 chip（无独立「已恢复」信号）。
    const wasReconnecting=this.reconnecting;
    this.reconnecting=false;
    const frameEpoch=typeof page.epoch==='string'?page.epoch:'';
    // epoch 换代（journal 重建/Run 删除重建）后序号口径重置：无条件采用帧内
    // sequence。只升不降守卫仅在同 epoch 下防回退；换代时若沿用旧高 lastSeq，
    // 新纪元的小序号会被判「落后」，每轮收 reset→scheduleRefetch(0) 风暴。
    const epochChanged=frameEpoch!==''&&this.epoch!==''&&frameEpoch!==this.epoch;
    if(page.reset){
      // 游标/epoch 失效后的整页重置：采用新游标并全量重读 RunDetail。
      if(frameEpoch)this.epoch=frameEpoch;
      if(typeof page.sequence==='number'&&(epochChanged||page.sequence>this.lastSeq))this.lastSeq=page.sequence;
      this.scheduleRefetch(0);
      return;
    }
    const beforeEpoch=this.epoch,beforeSeq=this.lastSeq;
    if(frameEpoch)this.epoch=frameEpoch;
    const rows=Array.isArray(page.rows)?page.rows:[];
    for(const event of rows){
      if(event&&typeof event.sequence==='number'&&(epochChanged||event.sequence>this.lastSeq))this.lastSeq=event.sequence;
      this.applyEvent(event);
    }
    if(typeof page.sequence==='number'&&(epochChanged||page.sequence>this.lastSeq))this.lastSeq=page.sequence;
    const meta=page.meta;
    let metaChanged=false;
    if(meta&&typeof meta==='object'){
      const serialized=JSON.stringify(meta);
      metaChanged=serialized!==this._lastMetaJSON;
      this._lastMetaJSON=serialized;
      this.meta=meta;
      if(meta.status)this.detail.status=meta.status;
      if(meta.stopReason!==undefined)this.detail.stopReason=meta.stopReason||'';
      if(meta.workspace)this.detail.workspace=meta.workspace;
    }
    // 变更检测守卫：epoch 未变、游标未前进、无事件行且 meta 内容未变的空帧直接
    // 跳过——无条件全量重渲染会在轮询传输（隧道主机）下每 ~200ms 重建 DOM、把
    // 滚动位置重置回顶部，终态 Run 还会每帧触发一次全量 RunDetail 重读。
    // 阶段二 meta 新增 mode/verify 等字段：只看游标不动会把 meta 变化静默丢弃
    // （如 verifying true→false 的闸门完成瞬间），故 meta 按序列化内容比较。
    if(beforeEpoch===this.epoch&&this.lastSeq===beforeSeq&&!rows.length&&!metaChanged){
      if(wasReconnecting)this.render();
      return;
    }
    this.render();
    const status=this.meta?.status||this.detail.status;
    if(TEAM_TERMINAL_RUN_STATUSES.has(status)){
      // 只在转入终态后补读一次完整 RunDetail；后续空帧不再重读。
      if(!this.terminalSynced){this.terminalSynced=true;this.scheduleRefetch(0);}
    }else{
      this.terminalSynced=false;
      if(rows.length)this.scheduleRefetch(400);
    }
  }
  applyEvent(event){
    if(!event||!this.detail)return;
    const payload=event.payload&&typeof event.payload==='object'?event.payload:{};
    // 契约：taskId 只在 payload 内（runner/dispatcher 的事件构造），顶层从未出现。
    const taskID=typeof payload.taskId==='string'?payload.taskId:'';
    if(!taskID)return;
    const tasks=this.detail.tasks||(this.detail.tasks=[]);
    const task=tasks.find(x=>x.id===taskID);
    if(!task)return; // 计划外/未知任务：交给整页重读兜底
    const status=TEAM_EVENT_TASK_STATUS[event.type];
    if(status)task.status=status;
    if(payload.submissionId)task.submissionId=payload.submissionId;
    // 阶段二 payload 增量：停靠分类（错误策略表）与 worktree 基线（diff 入口）。
    if(payload.outcome!==undefined)task.outcome=payload.outcome||'';
    if(payload.baseRevision!==undefined)task.baseRevision=payload.baseRevision||'';
    if(payload.error!==undefined)task.error=payload.error||'';
    if(payload.stopReason!==undefined)task.stopReason=payload.stopReason||'';
    if(event.type==='task_completed'){
      task.result={sessionId:payload.sessionId,turnId:payload.turnId,
        result:payload.result,resultChars:payload.resultChars,truncated:payload.truncated};
    }
  }
  scheduleRefetch(delay){
    clearTimeout(this.refetchTimer);
    this.refetchTimer=setTimeout(()=>this.refetch(),delay);
  }
  async refetch(){
    const generation=this.generation;
    if(!this.teamId||!this.runId)return;
    try{
      const detail=await this.request(this.runUrl(''));
      if(generation!==this.generation)return;
      this.detail=detail;
      this.refetchFailures=0;
      this.render();
    }catch{
      // 连接问题交给 transport 重连；此处有限重试，避免终态帧后的唯一一次
      // 补读失败把任务卡永远留在旧状态。
      if(generation!==this.generation||!this.stopFn)return;
      if(++this.refetchFailures<=5)this.scheduleRefetch(1000);
    }
  }
  leave(){
    this.close();
    this.onNavigateAway&&this.onNavigateAway();
  }
  close(){
    clearTimeout(this.refetchTimer);this.refetchTimer=null;
    if(this.stopFn){this.stopFn();this.stopFn=null;}
  }
  render(){
    const t=BridgeI18n.t,detail=this.detail;
    if(!detail)return;
    const root=this.root;
    // 滚动位置保持：全量重建会新建滚动容器 .team-run-body，先把旧容器的
    // scrollTop 带过去——否则每个事件帧都把用户甩回顶部，运行中的 Run 页
    // 基本无法滚动阅读。
    const prevBody=root.querySelector('.team-run-body');
    const scrollTop=prevBody?prevBody.scrollTop:0;
    root.replaceChildren();
    const head=this.node('header',undefined,'team-run-head');
    const back=this.button('‹');
    back.className='icon-button';
    back.setAttribute('aria-label',t('返回'));
    back.title=t('返回');
    const info=this.node('div',undefined,'team-run-info');
    info.append(this.node('h1',detail.goal||detail.id));
    const statusLine=this.node('p',undefined,'team-run-status');
    statusLine.append(this.runBadge(detail.status));
    if(detail.stopReason)statusLine.append(this.node('span',detail.stopReason,'team-stop-reason'));
    if(this.reconnecting){
      // 断线提示 chip：transport 重试期间置位，成功帧到达自清（对照聊天页
      // #status 的 offline 样式，但 Run 页 DOM 无 #status，就地展示）。
      statusLine.append(this.node('span',t('连接中断，重连中…'),'status offline'));
    }
    const verifyBadge=this.renderVerifyBadge();
    if(verifyBadge)statusLine.append(verifyBadge);
    if(detail.round)statusLine.append(this.node('span',t('第 ')+detail.round+t(' 轮'),'muted'));
    info.append(statusLine);
    head.append(back,info,this.renderActions());
    root.append(head);
    const body=this.node('div',undefined,'team-run-body');
    const counts=this.meta?.taskCounts;
    if(counts&&typeof counts==='object'){
      const summary=Object.entries(counts).map(([key,value])=>teamTaskStatusLabel(key)+' '+value).filter(Boolean).join(' · ');
      if(summary)body.append(this.node('p',summary,'muted'));
    }
    const workspace=detail.workspace;
    if(workspace&&typeof workspace==='object'){
      const line=this.node('p',undefined,'muted team-workspace');
      line.append(document.createTextNode(t('当前写入者：')));
      const holder=workspace.writeLockHeldBy||null;
      line.append(holder?this.node('code',holder):this.node('span',t('无')));
      body.append(line);
    }
    const tasks=detail.tasks||[];
    if(!tasks.length)body.append(this.node('p',t('还没有任务。'),'muted'));
    for(const task of tasks)body.append(this.renderTask(task));
    root.append(body);
    body.scrollTop=scrollTop;
  }
  /* 合并 RunDetail.verify（权威：command/outputHead/truncated）与随帧 meta.verify
   * （实时：verifying/exitCode 摘要）。postAction 后 meta 置空，回落 detail。 */
  verifyInfo(){
    const detailVerify=this.detail&&this.detail.verify&&typeof this.detail.verify==='object'?this.detail.verify:null;
    const metaVerify=this.meta&&this.meta.verify&&typeof this.meta.verify==='object'?this.meta.verify:null;
    if(!detailVerify&&!metaVerify)return null;
    const exitCode=typeof metaVerify?.exitCode==='number'?metaVerify.exitCode
      :(typeof detailVerify?.exitCode==='number'?detailVerify.exitCode:null);
    return {
      command:Array.isArray(detailVerify?.command)?detailVerify.command.join(' '):(detailVerify?.command||''),
      exitCode,
      verifying:!!metaVerify?.verifying,
      outputHead:detailVerify?.outputHead||'',
      truncated:!!detailVerify?.truncated,
    };
  }
  /* 验证闸门徽标（§6.4.1）：未跑显示命令（等宽）；verifying 复用 .working 脉冲点；
   * 完成显示退出码徽标（0 绿/非 0 红）+ outputHead 折叠展开。 */
  renderVerifyBadge(){
    const t=BridgeI18n.t;
    const verify=this.verifyInfo();
    if(!verify)return null;
    const box=this.node('span',undefined,'team-verify');
    if(verify.verifying){
      const working=this.node('span',undefined,'working');
      working.append(this.node('i'));
      working.append(document.createTextNode(t('验证中…')));
      box.append(working);
      return box;
    }
    if(typeof verify.exitCode==='number'){
      const badge=this.node('span',undefined,'team-badge '+(verify.exitCode===0?'b-completed':'b-failed'));
      badge.textContent=t('验证退出码')+' '+verify.exitCode;
      box.append(badge);
      if(verify.outputHead){
        const details=document.createElement('details');
        details.className='team-verify-output';
        const summary=document.createElement('summary');
        summary.textContent=t('验证输出');
        details.append(summary);
        details.append(this.node('pre',verify.outputHead,'task-result-text'));
        if(verify.truncated)details.append(this.node('p',t('输出已截断'),'muted'));
        box.append(details);
      }
      return box;
    }
    const badge=this.node('span',undefined,'team-badge b-ready');
    badge.append(document.createTextNode(t('验证')+' '));
    badge.append(this.node('code',verify.command));
    return badge;
  }
  renderActions(){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'team-run-actions');
    const status=this.detail.status;
    if(status==='waiting_input'){
      box.append(this.node('span',t('结果未知任务已核查，不再重发；继续可继续的分支。'),'muted team-resume-hint'));
      // 验证闸门失败停靠（stopReason 以 verify_failed 开头）时，resume 即重跑闸门。
      const verifyFailed=(this.detail.stopReason||'').startsWith('verify_failed');
      const resume=this.button(verifyFailed?t('重新验证'):t('继续'),async()=>{
        await this.postAction('resume');
      });
      box.append(resume);
    }
    if(!TEAM_TERMINAL_RUN_STATUSES.has(status)){
      const cancel=this.button(t('取消'),async()=>{
        await this.postAction('cancel');
      });
      box.append(cancel);
    }
    if(this.actionNotice)box.append(this.node('span',this.actionNotice,'team-notice'));
    if(this.actionError)box.append(this.node('span',this.actionError,'error'));
    return box;
  }
  async postAction(action){
    if(!this.detail)return;
    this.actionError='';this.actionNotice='';
    const runId=this.runId,teamId=this.teamId;
    try{
      const detail=await this.request(this.runUrl(action),{method:'POST'});
      if(teamId!==this.teamId||runId!==this.runId)return;
      this.detail=detail;this.meta=null;
      this.render();
    }catch(error){
      if(teamId!==this.teamId||runId!==this.runId)return;
      this.actionError=error.message;
      this.render();
    }
  }
  renderTask(task){
    const t=BridgeI18n.t;
    const member=this.teamMember(task.memberId);
    const card=this.node('section',undefined,'task-card');
    const head=this.node('div',undefined,'task-head');
    head.append(this.node('strong',task.id));
    head.append(this.node('span',(member?(member.name||member.id):task.memberId)+(member?' · '+BridgeI18n.agentLabel(member.agentKind):''),'task-member'));
    head.append(this.taskBadge(task.status));
    card.append(head);
    if(task.instructionsHead)card.append(this.node('p',task.instructionsHead,'task-instructions'));
    const metaBits=[];
    if(task.broadcastGroup)metaBits.push(t('并行组：')+task.broadcastGroup);
    if(task.dependsOn&&task.dependsOn.length)metaBits.push(t('依赖：')+task.dependsOn.join('、'));
    if(task.writeLock&&task.writeLock!=='none')metaBits.push(t('写锁：')+(task.writeLock==='exclusive'?t('独占'):task.writeLock));
    if(task.baseRevision)metaBits.push(t('基线：')+String(task.baseRevision).slice(0,12));
    if(this.detail.workspace?.writeLockHeldBy===task.id)metaBits.push(t('持有项目写锁'));
    if(metaBits.length)card.append(this.node('p',metaBits.join(' · '),'muted task-meta'));
    if(task.stopReason&&task.status!=='completed')card.append(this.node('p',t('停止原因：')+task.stopReason,'muted'));
    // 停靠分类（§6.3 错误策略表）：quota/auth/not_configured 等停靠的任务停在
    // 当前分支并等用户处理，提示原因而不自动重试。
    const outcomeLabel=task.outcome?TEAM_TASK_OUTCOME_LABELS[task.outcome]:'';
    if(outcomeLabel)card.append(this.node('p',t('停靠原因：')+t(outcomeLabel),'muted'));
    if(task.error)card.append(this.node('p',task.error,'error'));
    const result=task.result;
    if(result&&typeof result==='object'){
      const box=this.node('div',undefined,'task-result');
      const resultHead=this.node('p',undefined,'task-result-head');
      resultHead.append(this.node('strong',t('结果')));
      const chars=typeof result.resultChars==='number'?result.resultChars:String(result.result||'').length;
      resultHead.append(this.node('span',chars+' '+t('字符'),'muted'));
      if(result.truncated)resultHead.append(this.node('span',t('已截断'),'team-truncated'));
      box.append(resultHead);
      if(result.result)box.append(this.node('pre',result.result,'task-result-text'));
      card.append(box);
    }
    const actions=this.node('div',undefined,'task-actions');
    const sessionId=result?.sessionId||(member?member.sessionId:null);
    if(sessionId){
      const open=this.button(t('打开原会话'),()=>{
        // 打开会话前先退出 Run 页（停订阅、隐藏区块），再走全局 openChat。
        this.leave();
        this.openChat&&this.openChat(sessionId);
      });
      actions.append(open);
    }
    // 转发入口：服务端对终态 Run 一律 409（forward.go terminalRun 拒绝），
    // 只在 Run 未终态时展示——结果卡最常见的查看时点（Run 已完成/失败）上
    // 该按钮必然失败，是死入口（§11 第 6 条的转发入口以 Run 仍在交互态为前提）。
    if(task.status==='completed'&&!TEAM_TERMINAL_RUN_STATUSES.has(this.detail.status)){
      actions.append(this.button(t('转发到成员'),()=>this.openForwardDialog(task)));
    }
    if(task.workspaceMode==='isolated_worktree'&&task.baseRevision){
      actions.append(this.button(this.diffOpen.has(task.id)?t('收起改动'):t('查看改动'),()=>this.toggleTaskDiff(task)));
    }
    if(actions.childNodes.length)card.append(actions);
    if(this.diffOpen.has(task.id))card.append(this.renderTaskDiff(task));
    return card;
  }
  /* ---- worktree diff 审查（§6.4：第一版只展示、不自动合并）----
     复用 BridgeDiff.fileCards/open（只读；{path,diff} 入参与 unified 文本直接
     映射）。截断双口径：顶层 truncated 是整包提示条（文件列表上方），单文件
     files[].truncated 由 BridgeDiff 的文件级「已截断」标记消费——绝不把整包
     截断塞进每张文件卡误标未截断文件。 */
  async toggleTaskDiff(task){
    if(this.diffOpen.has(task.id)){
      this.diffOpen.delete(task.id);
      this.render();
      return;
    }
    this.diffOpen.add(task.id);
    // 终态任务的 diff 不再变化：命中缓存直接展示；运行中/失败任务每次展开
    // 重拉，避免展示陈旧截断内容。
    const cached=this.taskDiffs.get(task.id);
    const taskFinal=task.status&&['completed','failed','cancelled','stopped','outcome_unknown'].includes(task.status);
    if(cached&&!cached.loading&&cached.data&&taskFinal){
      this.render();
      return;
    }
    const generation=this.generation;
    this.taskDiffs.set(task.id,{loading:true});
    this.render();
    try{
      const data=await this.request(this.runUrl('tasks/'+encodeURIComponent(task.id)+'/diff'));
      if(generation!==this.generation)return;
      this.taskDiffs.set(task.id,{loading:false,data});
    }catch(error){
      if(generation!==this.generation)return;
      this.taskDiffs.set(task.id,{loading:false,error:error.message});
    }
    this.render();
  }
  renderTaskDiff(task){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'task-diff');
    const entry=this.taskDiffs.get(task.id);
    if(!entry||entry.loading){box.append(this.node('p',t('正在读取改动…'),'muted'));return box;}
    if(entry.error){box.append(this.node('p',entry.error,'error'));return box;}
    const data=entry.data&&typeof entry.data==='object'?entry.data:{};
    const files=Array.isArray(data.files)?data.files:[];
    if(data.truncated){
      const bar=this.node('p',undefined,'muted');
      bar.append(this.node('span',t('diff 过大已截断，仅显示部分文件。'),'team-truncated'));
      box.append(bar);
    }
    if(!files.length){box.append(this.node('p',t('暂无差异内容'),'muted'));return box;}
    if(typeof window.BridgeDiff==='undefined'){
      box.append(this.node('p',files.map(file=>file.path).join('\n'),'task-result-text'));
      return box;
    }
    // cwd 未知：文件路径按服务端输出原样展示；点开单文件走 BridgeDiff 单例弹窗，
    // 多文件靠文件卡列表逐个打开。
    box.append(window.BridgeDiff.fileCards(files,undefined,change=>window.BridgeDiff.open(change)));
    return box;
  }
  /* ---- 成员转发（§12 阶段二工作项 4）----
     轻量 <dialog class="picker">（照 runDialog/openQueueEditor 惯例；确认流与
     busy 禁用照 migrate.js 模式但独立实现——MigrateControl 装配绑死
     sessionUrl('migrate')，Run 页无「当前聊天」上下文，不可复用装配）。
     请求带客户端 UUID 作 id 幂等键：失败重试复用同 id 重放，字段变更时重生成；
     source 支持任务结果（sourceTaskId）与会话转录（sourceSessionId+scope）。 */
  forwardDialog(){
    if(this._forwardDialog)return this._forwardDialog;
    const dialog=document.createElement('dialog');
    dialog.id='team-forward-dialog';dialog.className='picker';
    dialog.setAttribute('aria-labelledby','team-forward-dialog-heading');
    const head=document.createElement('div');head.className='picker-head';
    const heading=document.createElement('h2');heading.id='team-forward-dialog-heading';
    const close=document.createElement('button');
    close.type='button';close.className='icon-button';close.textContent='×';
    close.setAttribute('aria-label',t_safe('关闭'));
    close.setAttribute('data-i18n-aria-label','关闭');
    close.onclick=()=>{dialog.forwardState=null;dialog.close();};
    head.append(heading,close);
    const body=document.createElement('div');body.className='picker-body';
    dialog.append(head,body);
    document.body.append(dialog);
    this._forwardDialog=dialog;
    return dialog;
  }
  openForwardDialog(task){
    const dialog=this.forwardDialog();
    const member=this.teamMember(task.memberId);
    dialog.forwardState={
      taskId:task.id,
      sourceSessionId:task.result?.sessionId||(member?member.sessionId:null),
      source:'task',scope:'main',
      target:'',note:'',
      busy:false,error:'',
      id:teamUUID(),
    };
    this.renderForwardDialog();
    dialog.showModal();
  }
  renderForwardDialog(){
    const dialog=this._forwardDialog,state=dialog&&dialog.forwardState;
    if(!state)return;
    const t=BridgeI18n.t;
    dialog.querySelector('h2').textContent=t('转发到成员');
    const body=dialog.querySelector('.picker-body');
    body.replaceChildren();
    const form=document.createElement('form');
    const sourceLabel=this.node('label');
    sourceLabel.append(this.node('span',t('来源')));
    const source=document.createElement('select');
    source.append(new Option(t('任务结果（有界摘要）'),'task'));
    const sessionOption=new Option(t('会话转录'),'session');
    if(!state.sourceSessionId)sessionOption.disabled=true;
    source.append(sessionOption);
    source.value=state.source;
    source.onchange=()=>{state.source=source.value;state.id=teamUUID();this.renderForwardDialog();};
    sourceLabel.append(source);
    form.append(sourceLabel);
    if(state.source==='session'){
      const scopeLabel=this.node('label');
      scopeLabel.append(this.node('span',t('转录范围')));
      const scope=document.createElement('select');
      for(const item of TEAM_FORWARD_SCOPES)scope.append(new Option(t(item.label),item.key));
      scope.value=state.scope;
      scope.onchange=()=>{state.scope=scope.value;state.id=teamUUID();};
      scopeLabel.append(scope);
      form.append(scopeLabel);
    }
    const targetLabel=this.node('label');
    targetLabel.append(this.node('span',t('目标成员')));
    const target=document.createElement('select');
    target.append(new Option(t('请选择成员…'),''));
    for(const m of (this.team?.members||[]).filter(m=>m.enabled!==false)){
      target.append(new Option((m.id===(this.team?.leadId||'')?t('负责')+' · ':'')+(m.name||m.id)+' · '+BridgeI18n.agentLabel(m.agentKind),m.id));
    }
    target.value=state.target;
    target.onchange=()=>{state.target=target.value;state.id=teamUUID();};
    targetLabel.append(target);
    form.append(targetLabel);
    const noteLabel=this.node('label');
    noteLabel.append(this.node('span',t('备注（可选，随转发文本一并注入）')));
    const note=document.createElement('textarea');
    note.rows=2;note.maxLength=2000;
    note.value=state.note;
    note.oninput=()=>{state.note=note.value;state.id=teamUUID();};
    noteLabel.append(note);
    form.append(noteLabel);
    if(state.error)form.append(this.node('p',state.error,'error'));
    const actions=this.node('div',undefined,'picker-actions');
    const submit=document.createElement('button');
    submit.type='submit';submit.className='primary';
    submit.textContent=t('转发');
    submit.disabled=state.busy;
    actions.append(submit);
    form.append(actions);
    form.onsubmit=async event=>{
      event.preventDefault();
      if(state.busy)return;
      state.error='';
      if(!state.target){state.error=t('请选择目标成员。');this.renderForwardDialog();return;}
      const body={id:state.id,targetMemberId:state.target};
      if(state.source==='session'){
        if(!state.sourceSessionId){state.error=t('没有可用的源会话。');this.renderForwardDialog();return;}
        body.sourceSessionId=state.sourceSessionId;
        body.scope=state.scope;
      }else body.sourceTaskId=state.taskId;
      if(state.note.trim())body.note=state.note.trim();
      const runId=this.runId;
      state.busy=true;this.renderForwardDialog();
      try{
        await this.request(this.runUrl('forward'),{method:'POST',body});
        if(runId!==this.runId)return;
        // task_forwarded 是 run 级事件（无 taskId）：applyEvent 早退，靠这里的
        // 主动补读刷新 Run 页；事件流 400ms 兜底是备份路径。
        dialog.forwardState=null;
        dialog.close();
        this.actionNotice=t('已转发，文本已进入目标成员的会话队列。');
        this.render();
        this.scheduleRefetch(0);
      }catch(error){
        if(runId!==this.runId)return;
        // 幂等键保留：直接点「转发」重试即同 id 重放，不会重复注入。
        state.busy=false;state.error=error.message;
        this.renderForwardDialog();
      }
    };
    body.append(form);
  }
}
if(typeof module!=='undefined')module.exports={TeamsPanel,TeamRunScreen};
