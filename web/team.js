'use strict';
/* Team 编排前端（阶段一 + 阶段二）：TeamsPanel 负责团队列表、新建/重命名/删除与
   Run 创建入口（照 PoolPanel 的 loading/error/render 全量重建模式）；TeamRunScreen
   挂在 #main 的 #team-run 区块内展示单个 Run 的任务卡与 salvage 结果，事件经
   BridgeTransport.subscribe 增量上屏（changes 长轮询 / SSE 同一套回调）。
   阶段二增量：broadcast 创建、成员转发、worktree diff 审查（复用 BridgeDiff，
   只读、不改 diff.js）、验证闸门徽标；meta 消费面扩展 mode/verify 摘要。
   阶段三增量：Lead 动作预览卡（detail.pendingActions 三态）、成员提问 decision
   卡（POST runUrl(decision)，questionId+version）、修订 Run 入口（amend 复用
   同一 runDialog 三段流，从 detail 预填）、任务缓存命中标记；顺路修阶段二
   遗留：verify 徽标改读顶层 verifyCommand、任务工作区清理入口。
   阶段四增量：阶段图（GET runUrl(graph) Kahn 分层，点击节点定位任务卡，
   404/空回退平铺列表）、统计卡（GET runUrl(stats)，coverage=none 明示无耗时
   记录、Token 固定文案）、paused 徽标与暂停/继续（同一 resume 端点）、
   run_stalled 顶部提示条、预算停靠（stopReason=budget_exhausted）继续即上调
   上限；TeamsPanel 增预算策略编辑（PATCH policy.budgetUnits）与「定时运行」
   卡（GET/PUT /schedule，模板编辑复用 runDialog 三段流，PUT 400 直出）。
   依赖 BridgeConnection 选择传输与 CSRF，以及 app.js 的 showLogin（仅运行时引用）。 */

const TEAM_ROLE_LABELS={lead:'负责人',member:'成员',reviewer:'评审'};
/* 任务停靠分类（task_stopped/task_failed payload 的 outcome，§6.3 错误策略表）：
   quota/auth/not_configured 类停靠落 run waiting_input，处理后 resume 重派。 */
const TEAM_TASK_OUTCOME_LABELS={rate_limited:'触发限流',quota:'额度不足',auth:'成员认证失败',not_configured:'成员未配置',context_overflow:'上下文超限',transient:'临时错误'};
// 停靠类 outcome（runner.go blockedStopReason）：Run/任务的 stopReason 是服务端中文整句。中文界面
// 照原文（保留「登录过期」等细节）；其他语言按停靠任务的结构化 outcome 组装，匹配不上再查固定词条。
const TEAM_BLOCKED_OUTCOMES=new Set(['quota','auth','not_configured']);
function teamRunStopReasonText(run){
  const t=BridgeI18n.t,reason=String(run&&run.stopReason||'');
  if(!BridgeI18n.language||BridgeI18n.language()==='zh')return reason;
  const task=(run&&run.tasks||[]).find(x=>x.status==='stopped'&&TEAM_BLOCKED_OUTCOMES.has(x.outcome)&&reason.startsWith('任务 '+x.id+' 停靠：'));
  if(task)return t('任务 ')+task.id+t(' 停靠：')+t(TEAM_TASK_OUTCOME_LABELS[task.outcome])+t('（处理账号或配置后可继续）');
  return t(reason);
}
/* broadcast 收集策略（§6.5）：all 保留每个分支结果；first_success 首个成功后取消其余。 */
const TEAM_COLLECT_OPTIONS=[
  {key:'all',label:'全部结果（保留每个分支的结果）'},
  {key:'first_success',label:'首个成功即止（成功后取消其余分支）'}];
/* 转发会话转录的范围（与迁移转录同口径：migrate.go main/full/latest）。 */
const TEAM_FORWARD_SCOPES=[
  {key:'main',label:'主要历史（标题与最近消息）'},
  {key:'full',label:'完整历史'},
  {key:'latest',label:'最新一轮提问与结果'}];
/* Lead 动作（§5.2）：delegate 委派成员任务、request_context 请求另一成员的
   已有结果、summarize 请求 Lead 汇总。状态三态：proposed 待执行（autoDispatch
   关闭的 Team 仅预览）、executed 已执行（可跳转派生任务）、rejected 已拒绝
   （reason 醒目展示）。拒绝与执行状态一律从 detail.pendingActions 读——
   action_* 是 run 级事件，applyEvent 早退不留 payload，400ms 兜底整页补读后
   detail 即权威。 */
const TEAM_ACTION_TYPE_LABELS={delegate:'委派任务',request_context:'请求上下文',summarize:'请求汇总'};
const TEAM_ACTION_STATUS_BADGES={proposed:{cls:'planned',label:'待执行'},executed:{cls:'completed',label:'已执行'},rejected:{cls:'failed',label:'已拒绝'}};
/* 客户端幂等键（Run 创建与成员转发）：crypto.randomUUID 不可用时降级；
   app.js 的 uuid() 属其模块内部，这里独立实现避免加载顺序耦合。 */
function teamUUID(){
  if(typeof crypto!=='undefined'&&typeof crypto.randomUUID==='function')return crypto.randomUUID();
  return Date.now().toString(16)+'-'+Math.random().toString(16).slice(2)+'-'+Math.random().toString(16).slice(2,10);
}
const TEAM_RUN_STATUS_LABELS={created:'已创建',dispatching:'派发中',collecting:'收集中',reducing:'汇总中',completed:'已完成',waiting_input:'等待确认',paused:'已暂停',failed:'已失败',cancelled:'已取消',stopped:'已停止',outcome_unknown:'结果未知'};
const TEAM_TASK_STATUS_LABELS={planned:'已规划',ready:'就绪',dispatching:'派发中',running:'运行中',completed:'已完成',failed:'已失败',cancelled:'已取消',outcome_unknown:'结果未知',stopped:'已停止'};
const TEAM_TERMINAL_RUN_STATUSES=new Set(['completed','failed','cancelled','stopped','outcome_unknown']);
/* 事件类型 → 任务状态（服务端事件行不携带 status 字段，状态由 type 推导；
   task_stopped 在用户取消 Run 时也用于 cancelled 任务——cancel 后的整页
   重读会立即校正。task_cache_hit 是 amend 缓存命中：任务直接携带父 Run 结果
   完成（§6.4.1）；action、question、invalid_action_block 事件的 payload.taskId
   指向来源任务、无任务状态语义，不进此表）。 */
const TEAM_EVENT_TASK_STATUS={task_dispatching:'dispatching',task_running:'running',task_completed:'completed',task_failed:'failed',task_stopped:'stopped',task_outcome_unknown:'outcome_unknown',task_retry_scheduled:'ready',task_cache_hit:'completed'};

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
/* 毫秒 → 短时长文案（统计卡与 stall 提示用）：<1min 以 0.1s 粒度、<1h 分秒、
   以上时分；后缀字母两种语言通用（照「第 N 轮」的拼接先例，不整句翻写）。 */
function teamDurationText(ms){
  if(typeof ms!=='number'||!isFinite(ms)||ms<0)return '';
  if(ms<60000)return (Math.round(ms/100)/10).toLocaleString(BridgeI18n.locale(),{maximumFractionDigits:1})+'s';
  const minutes=Math.floor(ms/60000);
  if(minutes<60)return minutes+'m '+Math.floor((ms%60000)/1000)+'s';
  return Math.floor(minutes/60)+'h '+(minutes%60)+'m';
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
  const response=await BridgeConnection.request(path,options);
  const data=await response.json().catch(()=>({}));
  if(!response.ok){
    if(response.status===401&&typeof showLogin==='function')showLogin();
    // 挂 HTTP 状态码：decision 端点按 409（问题已过期/已回答，整页补读）与
    // 400（选项/格式错误，保持卡可重答）分流；既有调用方只读 message 不受影响。
    const error=Error(data.error||(typeof t==='function'?t('请求失败'):'请求失败'));
    error.status=response.status;
    throw error;
  }
  return data;
}

/* Run 创建/修订对话框宿主（<dialog class="picker"> 动态创建，照 openQueueEditor
   惯例）：TeamsPanel（新建 Run）与 TeamRunScreen（创建修订 Run）共用同一模块
   级实例——同一时刻至多打开一个，状态挂 dialog.runState；提交端点与成功回调
   随 state 注入（submit/onSuccess），宿主自身不持有 request 依赖。第一屏填
   goal/mode/任务行（修订模式另有验证命令输入），第二屏按提交顺序+依赖做
   确认预览（§11 默认保留预览）；修订模式的预填行由调用方从旧 Run detail
   组装（broadcast 的逐成员展开任务先聚合回条目行）。 */
class TeamRunDialogHost{
  constructor(){this._dialog=null;}
  node(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
  button(text,fn){const n=this.node('button',text);n.type='button';n.onclick=fn;return n;}
  runDialog(){
    if(this._dialog)return this._dialog;
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
    this._dialog=dialog;
    return dialog;
  }
  open(state){
    const dialog=this.runDialog();
    state.phase='edit';state.busy=false;state.error='';state.parsed=[];state.requestId=null;
    dialog.runState=state;
    this.render();
    dialog.showModal();
  }
  /* 计划行 → 提交 plan（single 模式不用）：pipeline 单成员（memberId），
     broadcast 多成员（memberIds ≥2，服务端按 BroadcastGroup 展开为逐成员任务）。
     workspaceMode/workspaceId 由修订预填行透传（创建流程不产生这两个字段，
     留空即默认 shared_workspace）。 */
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
      if(row.workspaceMode)task.workspaceMode=row.workspaceMode;
      if(row.workspaceId)task.workspaceId=row.workspaceId;
      tasks.push(task);
    }
    return tasks;
  }
  render(){
    const dialog=this._dialog,state=dialog&&dialog.runState;
    if(!state)return;
    const t=BridgeI18n.t;
    dialog.querySelector('h2').textContent=state.phase==='preview'
      ?t('确认 Run 计划')
      :(state.schedule?t('定时运行配置'):(state.amend?t('创建修订 Run'):t('新建 Run')))+' · '+state.team.name;
    const body=dialog.querySelector('.picker-body');
    body.replaceChildren();
    const form=document.createElement('form');
    if(state.phase==='edit'){
      // 定时运行触发字段（schedule 模式独有，插在 goal 之前）：间隔与 cron
      // 二选一由触发方式下拉控制——契约要求 PUT 时恰一非空（都空/都非空 400），
      // 客户端用单输入规避「都非空」，间隔下限 300 先行本地校验。
      if(state.schedule){
        const enableLabel=this.node('label',undefined,'team-schedule-enable');
        const enableBox=document.createElement('input');
        enableBox.type='checkbox';
        enableBox.checked=state.schedule.enabled;
        enableBox.onchange=()=>{state.schedule.enabled=enableBox.checked;};
        enableLabel.append(enableBox);
        enableLabel.append(this.node('span',t('启用定时运行')));
        form.append(enableLabel);
        const kindLabel=this.node('label');
        kindLabel.append(this.node('span',t('触发方式')));
        const kind=document.createElement('select');
        kind.append(new Option(t('固定间隔（秒）'),'interval'));
        kind.append(new Option(t('cron 表达式（分 时 日 月 周）'),'cron'));
        kind.value=state.schedule.kind;
        kind.onchange=()=>{state.schedule.kind=kind.value;this.render();};
        kindLabel.append(kind);
        form.append(kindLabel);
        if(state.schedule.kind==='interval'){
          const intervalLabel=this.node('label');
          intervalLabel.append(this.node('span',t('间隔秒数（下限 300）')));
          const interval=document.createElement('input');
          interval.type='number';interval.min='300';interval.step='1';
          interval.value=state.schedule.interval;
          interval.oninput=()=>{state.schedule.interval=interval.value;};
          intervalLabel.append(interval);
          form.append(intervalLabel);
        }else{
          const cronLabel=this.node('label');
          cronLabel.append(this.node('span',t('cron 表达式')));
          const cron=document.createElement('input');
          cron.value=state.schedule.cron;cron.maxLength=64;
          cron.placeholder='*/30 9-18 * * 1-5';
          cron.oninput=()=>{state.schedule.cron=cron.value;};
          cronLabel.append(cron);
          form.append(cronLabel);
        }
      }
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
      mode.onchange=()=>{state.mode=mode.value;this.render();};
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
      if(state.amend||state.schedule){
        // 修订/定时模式独有：验证命令随 Run 声明（§6.4.1），修订从父 Run 的
        // verifyCommand 预填、定时从快照预填；创建流程不带此字段。
        const verifyLabel=this.node('label');
        verifyLabel.append(this.node('span',t('验证命令（可选，空格分隔，如 make check）')));
        const verify=document.createElement('input');
        verify.value=state.verifyText||'';
        verify.placeholder='make check';
        verify.oninput=()=>{state.verifyText=verify.value;};
        verifyLabel.append(verify);
        form.append(verifyLabel);
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
            const remove=this.button(t('移除'),()=>{state.tasks.splice(index,1);for(const other of state.tasks)other.deps.delete(row.id);this.render();});
            remove.className='plain member-remove';
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
                this.render();
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
                this.render();
              };
              deps.append(chip);
            }
            box.append(deps);
          }
          rowsBox.append(box);
        }
        form.append(rowsBox);
        const addTask=this.button(t('＋ 添加任务'),()=>{
          state.tasks.push({id:'t'+(state.tasks.length+1),memberId:'',memberIds:new Set(),deps:new Set(),prompt:'',writeLock:'none'});
          this.render();
        });
        addTask.className='team-add-member';
        form.append(addTask);
      }
    }else{
      const preview=this.node('ol',undefined,'team-plan-preview');
      const memberName=memberId=>{
        const member=(state.team.members||[]).find(m=>m.id===memberId);
        return member?(member.name||member.id):memberId;
      };
      if(state.schedule){
        const trigger=state.schedule.kind==='interval'
          ?t('每 ')+parseInt(state.schedule.interval,10)+t(' 秒')
          :t('cron：')+state.schedule.cron.trim();
        const item=this.node('li');
        item.append(this.node('strong',(state.schedule.enabled?t('启用中'):t('已停用'))+' · '+trigger));
        preview.append(item);
      }
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
          if(task.dependsOn?.length)bits.push(t('依赖：')+task.dependsOn.join(t('、')));
          bits.push(t('展开为：')+task.memberIds.map(memberName).join(t('、')));
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
          if(task.dependsOn?.length)bits.push(t('依赖：')+task.dependsOn.join(t('、')));
          meta.textContent=bits.join(' · ');
          item.append(meta);
          const head=task.prompt.length>80?task.prompt.slice(0,80)+'…':task.prompt;
          item.append(this.node('p',head,'task-instructions'));
          preview.append(item);
        }
      }
      form.append(preview);
      form.append(this.node('p',state.schedule
        ?t('保存后按此模板自动创建 Run；可随时停用或编辑。')
        :state.amend
        ?t('修订会创建一个新 Run；与父 Run 输入一致且无依赖变化的任务将直接复用其结果（缓存命中）。')
        :state.mode==='broadcast'
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
        this.render();
      });
      back.className='plain';
      back.disabled=state.busy;
      actions.append(back);
    }
    const submit=document.createElement('button');
    submit.type='submit';submit.className='primary';
    submit.textContent=state.phase==='preview'?(state.amend?t('创建修订 Run'):t('开始运行')):t('预览计划');
    submit.disabled=state.busy;
    actions.append(submit);
    form.append(actions);
    form.onsubmit=async event=>{
      event.preventDefault();
      if(state.busy)return;
      state.error='';
      if(!state.goal.trim()){state.error=t('请填写目标。');this.render();return;}
      if(state.phase==='edit'){
        if(state.schedule){
          // 触发字段本地校验（服务端同口径 400 兜底）：间隔 ≥300、cron 非空。
          if(state.schedule.kind==='interval'){
            const interval=parseInt(state.schedule.interval,10);
            if(!Number.isFinite(interval)||interval<300){state.error=t('定时间隔至少 300 秒。');this.render();return;}
          }else if(!state.schedule.cron.trim()){state.error=t('请填写 cron 表达式。');this.render();return;}
        }
        if(state.mode!=='single'){
          const tasks=this.parseRunTasks(state);
          if(!tasks){this.render();return;}
          state.parsed=tasks;
        }else state.parsed=[];
        state.phase='preview';
        this.render();
        return;
      }
      const payload=state.schedule
        ?{enabled:state.schedule.enabled===true,goal:state.goal.trim(),mode:state.mode}
        :{goal:state.goal.trim(),mode:state.mode};
      if(state.schedule&&state.schedule.kind==='interval')payload.intervalSecs=parseInt(state.schedule.interval,10);
      else if(state.schedule)payload.cron=state.schedule.cron.trim();
      if(state.mode!=='single'){
        payload.plan=state.parsed;
        // 服务端契约读取 collectPolicy（teams.go handleRunCreate 的
        // stringField(body,"collectPolicy")；测试 teams_test.go:407 同键）。
        if(state.mode==='broadcast')payload.collectPolicy=state.collect;
      }
      if(state.amend||state.schedule){
        // 验证命令（§6.4.1）：文本按空白切段（服务端固定 argv 白名单校验，
        // 非法段 400 直出）；空文本不携带该键，等同未声明。
        const argv=(state.verifyText||'').trim().split(/\s+/).filter(Boolean);
        if(argv.length)payload.verifyCommand=argv;
      }
      // 请求幂等键（可选 UUID）：失败后直接再点提交复用同 id 重放，不会在
      // 网络抖动下重复建 Run；返回编辑则重生成。Run 创建键名是 id（POST /runs
      // 契约），定时保存键名是 requestId（PUT /schedule 契约），两处不可混用。
      if(!state.requestId)state.requestId=teamUUID();
      if(state.schedule)payload.requestId=state.requestId;
      else payload.id=state.requestId;
      state.busy=true;this.render();
      try{
        const run=await state.submit(payload);
        dialog.runState=null;
        dialog.close();
        state.onSuccess&&state.onSuccess(run);
      }catch(error){
        state.busy=false;state.error=error.message;
        this.render();
      }
    };
    body.append(form);
  }
}
const teamRunDialogHost=new TeamRunDialogHost();

class TeamsPanel{
  constructor({root,request,openRun,openChat,onChange}){
    Object.assign(this,{root,request,openRun,openChat,onChange});
    this.teams=[];this.loading=false;this.busy=false;this.error='';this.notice='';
    this.create=null;       // 新建团队表单状态
    this.renaming=null;     // 重命名中的 teamId
    this.renameValue='';
    this.expanded=new Set(); // Run 列表展开的 teamId
    this.runs=new Map();    // teamId -> {loading,error,rows}
    this.scheduleOpen=new Set(); // 定时运行卡展开的 teamId
    this.schedules=new Map();    // teamId -> {loading,error,cfg,busy,notice}
    this.policyEdit=null;   // 预算策略编辑中的 teamId（§11.7，0=只统计）
    this.policyBudgetValue='';
    this.projects=null;     // 新建表单可用的项目空间（打开面板时快照）
    this.agents=[];         // 可创建团队成员会话的 ACP Agent
  }
  node(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
  button(text,fn){const n=this.node('button',text);n.type='button';n.onclick=fn;return n;}
  runBadge(status){return teamStatusBadge(status,TEAM_RUN_STATUS_LABELS);}
  async refresh(){
    if(this.loading)return;
    this.loading=true;this.error='';this.render();
    try{const value=await this.request('/api/teams');this.teams=value.teams||[];this.onChange&&this.onChange(this.teams);}
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
      this.create=this.create?null:{name:'',project:'',autoDispatch:true,error:'',requestId:teamUUID(),attempt:'',members:[{agent:'',role:'lead',name:''}]};
      if(this.create)this.loadProjects();
      this.render();
    });
    create.disabled=this.busy;
    toolbar.append(refresh,create);
    root.append(toolbar);
    if(this.error)root.append(this.node('p',this.error,'error'));
    if(this.notice){root.append(this.node('p',this.notice,'team-notice'));this.notice='';}
    if(this.loading&&!this.teams.length){root.append(this.node('p',t('正在读取团队…'),'muted'));return;}
    if(this.create)root.append(this.renderCreateForm());
    if(this.teams.length){
      for(const team of this.teams)root.append(this.renderTeam(team));
    }else if(!this.create){
      root.append(this.node('p',t('还没有团队。点击「新建团队」，按项目空间添加成员。'),'muted'));
    }
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
    // 项目名同侧栏取 cwd 末段（newProjectEntry 的 filepath.Base），无 cwd 才回落内部 key。
    const projectName=(team.projectCwd||'').replace(/\\/g,'/').replace(/\/+$/,'').split('/').pop();
    if(projectName||team.project)parts.push(t('项目 ')+(projectName||team.project));
    const updated=teamTimeText(team.updatedAt);
    if(updated)parts.push(t('更新于 ')+updated);
    meta.textContent=parts.join(' · ');
    card.append(meta);
    const chips=this.node('div',undefined,'team-members');
    for(const m of members){
      const chip=this.node('span',undefined,'team-member'+(m.available===false?' unavailable':'')+(m.enabled===false?' disabled':''));
      const role=t(TEAM_ROLE_LABELS[m.role||'member']||m.role);
      chip.textContent=(m.id===team.leadId?'★ ':'')+role+' · '+(m.name||m.id)+' · '+BridgeI18n.agentLabel(m.agentKind)
        +(m.available===false?' · '+t('不可用'):'')+(m.enabled===false?' · '+t('已停用'):'');
      chip.title=t('会话：')+(m.sessionId||'');
      chips.append(chip);
    }
    if(members.length)card.append(chips);
    const policy=team.policy||{};
    const budget=policy.budgetUnits;
    card.append(this.node('p',t('最大并行 ')+(policy.maxParallelTasks??'-')+' · '+t('最大轮次 ')+(policy.maxRounds??'-')+' · '+t('自动派发 ')+(policy.autoDispatch?t('开'):t('关'))
      +' · '+t('预算 ')+(typeof budget==='number'&&budget>0?budget:t('只统计')),'muted team-policy'));
    // 预算策略编辑（§11.7/§9「修订预算」的手机入口）：PATCH policy.budgetUnits
    // 走既有 team PATCH 通道（version 冲突 409 的中文文案由服务端透出）。
    if(this.policyEdit===team.id)card.append(this.renderPolicyEdit(team));
    const actions=this.node('div',undefined,'team-actions');
    const runsBtn=this.button(this.expanded.has(team.id)?t('收起 Run'):t('Run 记录'),()=>{
      if(this.expanded.has(team.id))this.expanded.delete(team.id);
      else{this.expanded.add(team.id);this.loadRuns(team.id);}
      this.render();
    });
    const createRun=this.button(t('新建 Run'),()=>this.openRunCreate(team));
    const openChat=this.button(t('打开团队聊天'),()=>this.openChat&&this.openChat(team));
    openChat.classList.add('primary');
    actions.append(openChat,runsBtn,createRun);
    actions.append(this.button(t('预算'),()=>{
      if(this.policyEdit===team.id)this.policyEdit=null;
      else{this.policyEdit=team.id;this.policyBudgetValue=typeof budget==='number'?String(budget):'0';}
      this.render();
    }));
    actions.append(this.button(this.scheduleOpen.has(team.id)?t('收起定时'):t('定时运行'),()=>{
      if(this.scheduleOpen.has(team.id))this.scheduleOpen.delete(team.id);
      else{this.scheduleOpen.add(team.id);this.loadSchedule(team.id);}
      this.render();
    }));
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
    if(this.scheduleOpen.has(team.id))card.append(this.renderSchedule(team));
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
      if(run.stopReason)row.title=t(teamRunStatusLabel(run.status))+' · '+teamRunStopReasonText(run);
      row.onclick=()=>this.openRun&&this.openRun(team.id,run.id);
      box.append(row);
    }
    return box;
  }
  /* ---- 预算策略编辑（§11.7）----
     内联一行数字输入（照重命名的就地编辑惯例）：0=只统计（Run 不因预算停靠，
     仍计数展示）；保存 PATCH {policy:{budgetUnits},version}——版本冲突等错误
     经 send() 落面板顶部 this.error，与重命名先例一致。 */
  renderPolicyEdit(team){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'team-policy-edit');
    const input=document.createElement('input');
    input.type='number';input.min='0';input.step='1';
    input.value=this.policyBudgetValue;
    input.oninput=()=>{this.policyBudgetValue=input.value;};
    const save=this.button(t('保存预算'),async()=>{
      const value=Number(this.policyBudgetValue);
      if(!Number.isInteger(value)||value<0){
        this.notice='';this.error=t('预算需为不小于 0 的整数（0=只统计）。');
        this.render();
        return;
      }
      const result=await this.send('PATCH','/api/teams/'+encodeURIComponent(team.id),{policy:{budgetUnits:value},version:team.version});
      if(result){this.policyEdit=null;await this.notify(t('预算策略已保存。'));}
    });
    const cancel=this.button(t('取消'),()=>{this.policyEdit=null;this.render();});
    box.append(input,save,cancel);
    const note=this.node('p',t('按任务派发次数计量；0 表示只统计、不因预算暂停。'),'muted team-budget-note');
    const wrap=this.node('div',undefined,'team-policy-edit-wrap');
    wrap.append(box,note);
    return wrap;
  }
  /* ---- 定时运行（§12 阶段四工作项 1 的配置入口）----
     GET /api/teams/:id/schedule 无配置 404（契约）→ 展示未配置态；已有配置
     展示摘要（启用/触发/nextDue/lastRun）+ lastRunId 直达 Run 页 + 编辑（复用
     runDialog 三段流的 schedule 模式）+ 启停快捷开关（PUT 全量模板，400 直出
     toast）。 */
  async loadSchedule(teamId){
    const entry={loading:true,error:'',cfg:null,busy:false,notice:''};
    this.schedules.set(teamId,entry);
    this.render();
    try{
      const result=await this.request('/api/teams/'+encodeURIComponent(teamId)+'/schedule');
      entry.cfg=(result&&result.schedule)||result||null;
    }catch(error){
      if(error.status===404)entry.cfg=null; // 未配置：契约里 GET 无配置即 404
      else entry.error=error.message;
    }finally{
      entry.loading=false;
      this.render();
    }
  }
  renderSchedule(team){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'team-schedule');
    box.append(this.node('h4',t('定时运行')));
    const entry=this.schedules.get(team.id);
    if(!entry||entry.loading){box.append(this.node('p',t('正在读取定时配置…'),'muted'));return box;}
    if(entry.error){box.append(this.node('p',entry.error,'error'));return box;}
    const cfg=entry.cfg;
    if(!cfg||typeof cfg!=='object'){
      box.append(this.node('p',t('未配置定时运行；可按固定间隔或 cron 自动创建 Run。'),'muted'));
      const setup=this.button(t('配置定时运行'),()=>this.openScheduleEdit(team,null));
      setup.disabled=this.busy;
      box.append(setup);
      return box;
    }
    const bits=[cfg.enabled?t('启用中'):t('已停用')];
    if(cfg.intervalSecs)bits.push(t('每 ')+cfg.intervalSecs+t(' 秒'));
    else if(cfg.cron)bits.push(t('cron：')+cfg.cron);
    const next=teamTimeText(cfg.nextDue);
    if(next)bits.push(t('下次 ')+next);
    const last=teamTimeText(cfg.lastRunAt);
    if(last)bits.push(t('上次 ')+last);
    box.append(this.node('p',bits.join(' · '),'team-schedule-summary'));
    if(cfg.lastRunId){
      const lastBtn=this.button(t('上次 Run：')+cfg.lastRunId);
      lastBtn.className='task-dep-chip';
      lastBtn.onclick=()=>this.openRun&&this.openRun(team.id,cfg.lastRunId);
      box.append(lastBtn);
    }
    const actions=this.node('div',undefined,'team-actions');
    const edit=this.button(t('编辑'),()=>this.openScheduleEdit(team,cfg));
    const toggle=this.button(cfg.enabled?t('停用'):t('启用'),()=>this.toggleSchedule(team,cfg));
    toggle.disabled=entry.busy||this.busy;
    actions.append(edit,toggle);
    box.append(actions);
    if(entry.notice){box.append(this.node('p',entry.notice,'team-notice'));entry.notice='';}
    return box;
  }
  /* 快捷启停：PUT 全量模板（enabled 翻转，其余原样带回）；契约要求 PUT 校验
     全字段，400（间隔/cron/goal 等）直接 toast + 卡内提示。 */
  async toggleSchedule(team,cfg){
    const t=BridgeI18n.t;
    const entry=this.schedules.get(team.id);
    if(!entry||entry.busy)return;
    entry.busy=true;this.render();
    const body={enabled:cfg.enabled!==true,goal:cfg.goal,mode:cfg.mode,requestId:teamUUID()};
    if(cfg.intervalSecs)body.intervalSecs=cfg.intervalSecs;
    if(cfg.cron)body.cron=cfg.cron;
    if(cfg.collectPolicy)body.collectPolicy=cfg.collectPolicy;
    if(Array.isArray(cfg.verifyCommand)&&cfg.verifyCommand.length)body.verifyCommand=cfg.verifyCommand;
    if(Array.isArray(cfg.plan))body.plan=cfg.plan;
    try{
      const result=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/schedule',{method:'PUT',body});
      entry.cfg=(result&&result.schedule)||result;
      entry.notice=t('定时配置已更新。');
    }catch(error){
      if(typeof toast==='function')toast(error.message); // app.js 全局（运行时可用）
      entry.notice=error.message;
    }finally{
      entry.busy=false;
      this.render();
    }
  }
  /* 模板编辑：复用 runDialog 三段流（edit→preview→PUT）的 schedule 模式，
     从 GET 快照预填 goal/mode/collect/plan/verifyCommand 与触发字段。 */
  openScheduleEdit(team,cfg){
    const schedule={
      enabled:cfg?cfg.enabled===true:true,
      kind:cfg&&cfg.cron&&!cfg.intervalSecs?'cron':'interval',
      interval:cfg&&cfg.intervalSecs?String(cfg.intervalSecs):'',
      cron:cfg&&cfg.cron?String(cfg.cron):'',
    };
    const mode=['single','pipeline','broadcast'].includes(cfg&&cfg.mode)?cfg.mode:'single';
    const tasks=mode==='single'?[]:(cfg&&Array.isArray(cfg.plan)?cfg.plan:[]).map(row=>({
      id:row.id||'',memberId:row.memberId||'',memberIds:new Set(row.memberIds||[]),
      deps:new Set(row.dependsOn||[]),prompt:row.prompt||'',writeLock:row.writeLock||'none',
      workspaceMode:row.workspaceMode||'',workspaceId:row.workspaceId||'',
    }));
    const teamId=team.id;
    teamRunDialogHost.open({
      team,schedule,
      goal:(cfg&&cfg.goal)||'',mode,collect:(cfg&&cfg.collectPolicy)||'all',tasks,
      verifyText:cfg&&Array.isArray(cfg.verifyCommand)?cfg.verifyCommand.join(' '):'',
      submit:payload=>this.request('/api/teams/'+encodeURIComponent(teamId)+'/schedule',{method:'PUT',body:payload}),
      onSuccess:()=>{this.loadSchedule(teamId);this.notify(t_safe('定时配置已保存。'));},
    });
  }
  // 团队成员与新建聊天一致：选择项目空间和 Agent，由网关创建专用 ACP 会话。
  async loadProjects(){
    if(this.projects||this._projectsLoading)return;
    this._projectsLoading=true;
    try{
      const result=await this.request('/api/projects');
      this.projects=(result.projects||[]).filter(row=>row.cwd);
      this.agents=(result.agents||[]).filter(row=>row.kind&&row.kind!=='codex');
    }catch(error){this.projects=[];this.agents=[];if(this.create)this.create.error=error.message;}
    finally{
      this._projectsLoading=false;
      if(this.create)this.render();
    }
  }
  projectLabel(row){return (row.name||row.cwd)+' · '+row.cwd;}
  renderCreateForm(){
    const t=BridgeI18n.t,create=this.create,projects=this.projects,agents=this.agents;
    const roleHelp={lead:t('接收团队目标，协调任务并汇总结果。'),member:t('接收分派的任务，在项目空间中执行。'),reviewer:t('检查其他成员的结果并给出评审意见。')};
    const form=this.node('form',undefined,'team-create');
    form.append(this.node('h3',t('新建团队')));
    form.append(this.node('p',t('无需先创建对话。团队共用一个项目空间；为每位成员选择执行 Agent，网关会自动创建专用会话。'),'muted team-create-intro'));
    const nameLabel=this.node('label');
    nameLabel.append(this.node('span',t('团队名称')));
    const nameInput=document.createElement('input');
    nameInput.value=create.name;nameInput.maxLength=120;nameInput.placeholder=t('例如：App 开发团队');
    nameInput.oninput=()=>{create.name=nameInput.value;};
    nameLabel.append(nameInput);
    form.append(nameLabel);
    const projectLabel=this.node('label',undefined,'team-project-field');
    projectLabel.append(this.node('span',t('项目空间')));
    const projectSelect=document.createElement('select');
    if(!projects){
      projectSelect.disabled=true;projectSelect.append(new Option(t('正在读取项目空间…'),''));
    }else if(!projects.length){
      projectSelect.disabled=true;projectSelect.append(new Option(t('没有可用的项目空间'),''));
    }else{
      projectSelect.append(new Option(t('请选择项目空间…'),''));
      for(const project of projects)projectSelect.append(new Option(this.projectLabel(project),project.key));
      projectSelect.value=create.project||'';
    }
    projectSelect.onchange=()=>{create.project=projectSelect.value;this.render();};
    projectLabel.append(projectSelect);form.append(projectLabel);
    const selectedProject=projects?.find(project=>project.key===create.project);
    if(selectedProject)form.append(this.node('p',selectedProject.cwd,'muted team-project-path'));
    const autoLabel=this.node('label',undefined,'notification-option');
    const autoInput=document.createElement('input');autoInput.type='checkbox';autoInput.checked=create.autoDispatch!==false;
    autoInput.onchange=()=>{create.autoDispatch=autoInput.checked;};
    autoLabel.append(autoInput,this.node('span',t('允许负责人自动派发任务')));form.append(autoLabel);
    const memberHeading=this.node('div',undefined,'team-create-section-title');
    memberHeading.append(this.node('strong',t('团队成员')));
    memberHeading.append(this.node('span',t('至少一位负责人；可继续添加成员或评审。'),'muted'));
    form.append(memberHeading);
    const rowsBox=this.node('div',undefined,'team-member-rows');
    for(const [index,row] of create.members.entries()){
      const box=this.node('section',undefined,'member-row');
      const head=this.node('div',undefined,'member-row-title');
      head.append(this.node('strong',t('成员 ')+(index+1)+' · '+t(TEAM_ROLE_LABELS[row.role]||row.role)));
      if(create.members.length>1){
        const remove=this.button(t('移除'),()=>{create.members.splice(index,1);this.render();});
        remove.className='plain member-remove';head.append(remove);
      }
      box.append(head);
      const agentLabel=this.node('label');
      agentLabel.append(this.node('span',t('执行 Agent')));
      const agentSelect=document.createElement('select');
      if(!projects){agentSelect.disabled=true;agentSelect.append(new Option(t('正在读取 Agent…'),''));}
      else if(!agents.length){agentSelect.disabled=true;agentSelect.append(new Option(t('没有可用的 ACP Agent'),''));}
      else{
        if(agents.length===1&&!row.agent)row.agent=agents[0].kind;
        if(agents.length>1)agentSelect.append(new Option(t('请选择 Agent…'),''));
        for(const agent of agents)agentSelect.append(new Option(agent.label||agent.kind,agent.kind));
        agentSelect.value=row.agent||'';
      }
      agentSelect.onchange=()=>{row.agent=agentSelect.value;};
      agentLabel.append(agentSelect);box.append(agentLabel);
      const roleLabel=this.node('label');
      roleLabel.append(this.node('span',t('角色')));
      const roleSelect=document.createElement('select');
      for(const [value,label] of Object.entries(TEAM_ROLE_LABELS))roleSelect.append(new Option(t(label),value));
      roleSelect.value=row.role;
      roleSelect.onchange=()=>{row.role=roleSelect.value;this.render();};
      roleLabel.append(roleSelect);box.append(roleLabel);
      box.append(this.node('p',roleHelp[row.role],'muted member-role-help'));
      const nameLabel2=this.node('label');
      nameLabel2.append(this.node('span',t('备注名（可选）')));
      const nameField=document.createElement('input');
      nameField.value=row.name||'';nameField.maxLength=120;nameField.placeholder=t('例如：后端开发');
      nameField.oninput=()=>{row.name=nameField.value;};
      nameLabel2.append(nameField);box.append(nameLabel2);
      rowsBox.append(box);
    }
    form.append(rowsBox);
    if(create.error)form.append(this.node('p',create.error,'error'));
    const actions=this.node('div',undefined,'team-create-actions');
    const add=this.button(t('＋ 添加成员'),()=>{
      create.members.push({agent:agents.length===1?agents[0].kind:'',role:'member',name:''});
      this.render();
    });
    add.className='team-add-member';
    const submit=document.createElement('button');
    submit.type='submit';submit.className='primary';submit.textContent=this.busy?t('正在创建…'):t('创建团队');
    submit.disabled=this.busy||!projects||!projects.length||!agents.length;
    actions.append(add,submit);form.append(actions);
    form.onsubmit=async event=>{
      event.preventDefault();
      if(this.busy)return;
      create.error='';
      const name=create.name.trim();
      if(!name){create.error=t('请填写团队名称。');this.render();return;}
      if(!create.project){create.error=t('请选择团队的项目空间。');this.render();return;}
      const members=[];
      for(const [i,item] of create.members.entries()){
        if(!item.agent){create.error=t('请为每位成员选择执行 Agent。');this.render();return;}
        const member={id:'member-'+(i+1),project:create.project,agent:item.agent,role:item.role};
        if(item.name.trim())member.name=item.name.trim();
        members.push(member);
      }
      const lead=members.find(m=>m.role==='lead')||members[0];
      const payload={name,project:create.project,members,leadId:lead.id,policy:{autoDispatch:create.autoDispatch!==false}},fingerprint=JSON.stringify(payload);
      if(create.attempt!==fingerprint){create.requestId=teamUUID();create.attempt=fingerprint;}
      payload.id=create.requestId;
      const team=await this.send('POST','/api/teams',payload);
      if(team){
        this.create=null;
        await this.notify(team.duplicate?t('团队已存在（内容一致），未重复创建。'):t('团队已创建。'));
        this.openChat&&this.openChat(team);
      }
    };
    return form;
  }
  /* Run 创建入口：三段流对话框（edit→preview→提交）由模块级 teamRunDialogHost
     承载（修订 Run 复用同一宿主，见 TeamRunScreen.openAmend）；提交端点与成功
     回调随 state 注入，宿主不感知调用方。 */
  openRunCreate(team){
    teamRunDialogHost.open({
      team,
      goal:'',mode:'single',collect:'all',
      tasks:[{id:'t1',memberId:'',memberIds:new Set(),deps:new Set(),prompt:'',writeLock:'none'}],
      submit:payload=>this.request('/api/teams/'+encodeURIComponent(team.id)+'/runs',{method:'POST',body:payload}),
      onSuccess:run=>{this.openRun&&this.openRun(team.id,run.id);this.loadRuns(team.id);},
    });
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
    this.questionState=new Map(); // questionId -> {busy,error,draft}：decision 卡本地态（帧重渲染不丢已填内容与错误）
    this.taskCleanup=new Map();   // taskId -> {busy,error,done}：工作区清理的就地反馈（错误照 forwardDialog 内联风格）
    this.taskActionKeys=new Map();// taskId -> actionKey：派生任务落地凭据（task_planned 载荷；Run detail 的任务对象不携带该字段）
    this.stats=null;         // /stats 快照：{loading,error,data}（进页拉一次+手动刷新）
    this.graph=null;         // /graph 快照：{loading,error,data}（404/空回退平铺任务列表）
    this.graphOpen=new Set(); // 阶段图聚合节点展开的 node id（帧重渲染不丢展开态）
    this.stalled=null;       // run_stalled 事件载荷 {sinceMs,lastCompletedTaskId}：任一 task_* 事件即清
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
    this.questionState.clear();this.taskCleanup.clear();this.taskActionKeys.clear();
    this.stats=null;this.graph=null;this.graphOpen.clear();this.stalled=null;
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
      // 阶段四补充视图：图/统计各拉一次（generation 守卫内防串台）；404（旧
      // 网关无该端点）时静默回退——图缺省即平铺任务列表，统计缺省不显示区块。
      this.loadGraph();
      this.loadStats();
    }catch(error){
      if(generation!==this.generation)return;
      this.root.replaceChildren();
      const body=this.node('div',undefined,'team-run-body');
      body.append(this.node('p',error.message,'error'));
      const back=this.button(BridgeI18n.t('返回'),()=>this.leave());back.className='plain';
      body.append(back);
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
      // stall 提示条（§6.3）：run_stalled 到达置位；任何 task_* 事件都代表编排
      // 仍在推进，即撤下。重开 Run 页经 after=-1 回放历史事件会自然恢复该态。
      if(event&&event.type==='run_stalled'){
        const stallPayload=event.payload&&typeof event.payload==='object'?event.payload:{};
        this.stalled={
          sinceMs:typeof stallPayload.sinceMs==='number'?stallPayload.sinceMs:null,
          lastCompletedTaskId:typeof stallPayload.lastCompletedTaskId==='string'?stallPayload.lastCompletedTaskId:'',
        };
      }else if(event&&typeof event.type==='string'&&event.type.indexOf('task_')===0){
        this.stalled=null;
      }
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
    // meta 只带未决问题计数（无文本，§5.4）：与本地 detail 的 open 数不一致说明
    // 快照落后于 meta（事件行被环形缓冲裁掉等），补一次整页读校准——问题的
    // 权威展示源是 detail.pendingQuestions，不进 meta。
    if(typeof this.meta?.pendingQuestions==='number'){
      const openNow=(this.detail.pendingQuestions||[]).filter(question=>(question.status||'open')==='open').length;
      if(this.meta.pendingQuestions!==openNow)this.scheduleRefetch(400);
    }
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
    // 派生任务落地凭据（§12 阶段三执行流）：task_planned 载荷携带 actionKey，而
    // Run detail 的任务对象不外发该字段——先于下方「未知任务早退」记账内存映射
    // taskId→actionKey，供 executed 动作卡的「派生任务」跳转；页面打开时
    // after=-1 的全量事件行（环形缓冲近 500 条）回放会补齐，被裁剪的旧历史
    // 无链接、只显示徽标。task_forwarded 虽也载 actionKey 但是 run 级事件
    // （无 taskId），无法进此映射。
    if(event.type==='task_planned'&&taskID&&typeof payload.actionKey==='string'&&payload.actionKey){
      this.taskActionKeys.set(taskID,payload.actionKey);
    }
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
    if(event.type==='task_completed'||event.type==='task_cache_hit'){
      task.result={sessionId:payload.sessionId,turnId:payload.turnId,
        result:payload.result,resultChars:payload.resultChars,truncated:payload.truncated};
      // amend 缓存命中（§6.4.1）：任务携带父 Run 结果直接完成，cachedFrom 指向
      // 父任务；就地补标记供任务卡显示，400ms 兜底整页补读后以 detail 为准。
      if(event.type==='task_cache_hit'&&payload.parentTaskId)task.cachedFrom=payload.parentTaskId;
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
      // 拿到权威快照后解除 decision 卡的 busy：已作答的问题转灰态；仍 open 的
      // 可重新作答（答案只允许消费一次，重复提交由服务端 409 拦截后整页补读）。
      for(const entry of this.questionState.values())entry.busy=false;
      this.render();
    }catch{
      // 连接问题交给 transport 重连；此处有限重试，避免终态帧后的唯一一次
      // 补读失败把任务卡永远留在旧状态。
      if(generation!==this.generation||!this.stopFn)return;
      if(++this.refetchFailures<=5)this.scheduleRefetch(1000);
    }
  }
  /* ---- 阶段图（§11.3，GET runUrl(graph)）----
     节点卡：状态徽标（单任务节点——非聚合且 runCount===1——用 detail 实时
     状态覆盖服务端快照；修复链聚合节点 runCount>1 时以服务端归并状态为准，
     本地 detail 只含根任务本人的陈旧终态，覆盖会误显；动作派生任务各自成
     节点）、成员 agentKind 徽标、
     runCount「×N」（循环阶段只显示一个稳定节点+运行次数）、依赖以文本表达；
     broadcast/autoPlan 聚合节点（memberIds>1）点击展开成员小列表，普通节点
     点击 scrollIntoView 高亮对应任务卡（revealTask）。 */
  loadGraph(){
    const generation=this.generation;
    this.graph={loading:true};
    this.request(this.runUrl('graph')).then(data=>{
      if(generation!==this.generation)return;
      this.graph={loading:false,data};
      this.render();
    }).catch(error=>{
      if(generation!==this.generation)return;
      // 404=旧快照/旧网关无图数据：静默回退平铺任务列表；其余错误小字提示。
      this.graph={loading:false,error:error.status===404?null:error.message};
      if(this.graph.error)this.render();
    });
  }
  /* ---- 统计（GET runUrl(stats)）----
     总耗时/每任务耗时条（相对最长任务的比例）/预算 used/ceiling；coverage=none
     明示旧版本数据无耗时记录；tokens 恒 null，固定文案「按任务归因暂不支持」。 */
  loadStats(){
    const generation=this.generation;
    this.stats={loading:true};
    this.render();
    this.request(this.runUrl('stats')).then(data=>{
      if(generation!==this.generation)return;
      this.stats={loading:false,data};
      this.render();
    }).catch(error=>{
      if(generation!==this.generation)return;
      // 404=旧网关无 stats 端点：整块隐藏（progressive）；其余错误就地提示。
      this.stats=error.status===404?null:{loading:false,error:error.message};
      this.render();
    });
  }
  renderStats(){
    const t=BridgeI18n.t;
    const box=this.node('section',undefined,'team-stats');
    const head=this.node('div',undefined,'team-stats-head');
    head.append(this.node('h3',t('统计')));
    const refresh=this.button(t('刷新'),()=>this.loadStats());
    refresh.disabled=this.stats.loading;
    head.append(refresh);
    box.append(head);
    if(this.stats.loading){box.append(this.node('p',t('正在读取统计…'),'muted'));return box;}
    if(this.stats.error){box.append(this.node('p',this.stats.error,'error'));return box;}
    const data=this.stats.data&&typeof this.stats.data==='object'?this.stats.data:{};
    if(data.coverage==='none'){
      box.append(this.node('p',t('旧版本数据无耗时记录。'),'muted'));
    }else{
      const bits=[];
      if(typeof data.runElapsedMs==='number')bits.push(t('总耗时 ')+teamDurationText(data.runElapsedMs));
      if(typeof data.dispatchCount==='number')bits.push(t('派发 ')+data.dispatchCount+t(' 次'));
      if(bits.length)box.append(this.node('p',bits.join(' · '),'muted'));
      const rows=Array.isArray(data.tasks)?data.tasks:[];
      if(rows.length){
        const grid=this.node('div',undefined,'team-stats-tasks');
        const max=Math.max(0,...rows.map(row=>typeof row.elapsedMs==='number'?row.elapsedMs:0));
        for(const row of rows){
          const line=this.node('div',undefined,'team-stat-row');
          const label=this.node('span',undefined,'team-stat-label');
          label.append(document.createTextNode(row.taskId||''));
          if(typeof row.attempt==='number'&&row.attempt>1)label.append(document.createTextNode(' · ×'+row.attempt));
          const bar=this.node('div',undefined,'team-stat-bar');
          const fill=this.node('i');
          if(typeof row.elapsedMs==='number'&&max>0)fill.style.width=Math.max(2,Math.round(row.elapsedMs/max*100))+'%';
          bar.append(fill);
          line.append(label,bar,this.node('span',teamDurationText(row.elapsedMs)||'—','team-stat-value'));
          grid.append(line);
        }
        box.append(grid);
      }
    }
    const budget=data.budget;
    if(budget&&typeof budget==='object'){
      const line=this.node('p',undefined,'muted');
      const used=typeof budget.used==='number'?budget.used:0;
      if(typeof budget.ceiling==='number'&&budget.ceiling>0){
        line.append(document.createTextNode(t('预算：')+used+'/'+budget.ceiling));
      }else{
        line.append(document.createTextNode(t('预算：只统计，已用 ')+used+t(' 次')));
      }
      if(budget.exhausted)line.append(this.node('span',t('预算已用尽'),'team-truncated'));
      box.append(line);
    }
    box.append(this.node('p',t('Token 按任务归因暂不支持。'),'muted'));
    return box;
  }
  renderGraph(){
    if(!this.graph||this.graph.loading||this.graph.error||!this.graph.data)return null;
    const nodes=Array.isArray(this.graph.data.nodes)?this.graph.data.nodes.filter(node=>node&&typeof node.id==='string'):[];
    if(!nodes.length)return null;
    const t=BridgeI18n.t;
    const box=this.node('section',undefined,'team-graph');
    box.append(this.node('h3',t('阶段图')));
    const layers=this.graphLayers(nodes);
    for(const [index,layer] of layers.entries()){
      if(index){
        const arrow=this.node('div',undefined,'team-graph-arrow');
        arrow.textContent='↓';
        box.append(arrow);
      }
      const row=this.node('div',undefined,'team-graph-layer');
      for(const node of layer)row.append(this.renderGraphNode(node));
      box.append(row);
    }
    return box;
  }
  /* Kahn 分层：每层一行，节点入层当且仅当其（在节点集内的）依赖全部落在更早
     层。显式 Plan 由 planner 拒绝环；防御性兜底把无法分层的剩余节点按原顺序
     追加为最后一行，绝不因图数据异常阻塞整页渲染。 */
  graphLayers(nodes){
    const byId=new Map(nodes.map(node=>[node.id,node]));
    const remaining=[...byId.keys()];
    const placed=new Set();
    const layers=[];
    while(remaining.length){
      const layerIDs=remaining.filter(id=>{
        const deps=(byId.get(id).dependsOn||[]).filter(dep=>byId.has(dep));
        return deps.every(dep=>placed.has(dep));
      });
      if(!layerIDs.length){
        layers.push(remaining.map(id=>byId.get(id)));
        break;
      }
      for(const id of layerIDs){
        remaining.splice(remaining.indexOf(id),1);
        placed.add(id);
      }
      layers.push(layerIDs.map(id=>byId.get(id)));
    }
    return layers;
  }
  renderGraphNode(node){
    const t=BridgeI18n.t;
    const aggregate=Array.isArray(node.memberIds)&&node.memberIds.length>1;
    // 本地 detail 实时覆盖仅限单任务节点（非聚合且 runCount===1）：派生链/
    // 修复链聚合节点的 id=根任务 ID，findTask 命中的是根任务本人的陈旧终态，
    // 会盖掉服务端按「链上最新执行」归并出的 node.status——循环阶段进行中
    //（根任务已终态、派生任务 running）会误显「已完成 ×N」而非「进行中」。
    const single=!aggregate&&!(typeof node.runCount==='number'&&node.runCount>1);
    const detailTask=single?this.findTask(node.id):null;
    const card=this.node('div',undefined,'team-graph-node'+(aggregate?' aggregate':''));
    card.setAttribute('role','button');
    card.tabIndex=0;
    const activate=()=>{
      if(aggregate){
        if(this.graphOpen.has(node.id))this.graphOpen.delete(node.id);
        else this.graphOpen.add(node.id);
        this.render();
        return;
      }
      this.revealTask(node.id);
    };
    card.onclick=activate;
    card.onkeydown=event=>{
      if(event.key==='Enter'||event.key===' '){event.preventDefault();activate();}
    };
    const head=this.node('div',undefined,'team-graph-node-head');
    head.append(this.node('strong',node.label||node.id));
    if(typeof node.runCount==='number'&&node.runCount>1)head.append(this.node('span','×'+node.runCount,'team-graph-runs'));
    head.append(this.taskBadge(detailTask?detailTask.status:node.status));
    card.append(head);
    const kinds=Array.isArray(node.agentKinds)?node.agentKinds:[];
    if(kinds.length){
      const chips=this.node('div',undefined,'team-graph-kinds');
      for(const kind of kinds)chips.append(this.node('span',BridgeI18n.agentLabel(kind),'team-graph-kind'));
      card.append(chips);
    }
    if(Array.isArray(node.dependsOn)&&node.dependsOn.length){
      card.append(this.node('p',t('依赖：')+node.dependsOn.join(t('、')),'muted team-graph-deps'));
    }
    if(node.resultHead){
      card.append(this.node('p',node.resultHead,'task-instructions'));
      if(node.truncated)card.append(this.node('span',t('已截断'),'team-truncated'));
    }
    if(aggregate&&this.graphOpen.has(node.id)){
      const list=this.node('div',undefined,'team-graph-members');
      for(const memberId of node.memberIds){
        const member=this.teamMember(memberId);
        list.append(this.node('span',(member?(member.name||member.id):memberId)
          +(member?' · '+BridgeI18n.agentLabel(member.agentKind):''),'team-graph-member'));
      }
      card.append(list);
    }
    return card;
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
    const back=this.button('‹',()=>this.leave());
    back.className='icon-button';
    back.setAttribute('aria-label',t('返回'));
    back.title=t('返回');
    const info=this.node('div',undefined,'team-run-info');
    info.append(this.node('h1',detail.goal||detail.id));
    const statusLine=this.node('p',undefined,'team-run-status');
    statusLine.append(this.runBadge(detail.status));
    if(detail.scheduledFrom)statusLine.append(this.node('span',t('定时触发'),'team-badge b-ready'));
    if(detail.stopReason){const reason=this.node('span',teamRunStopReasonText(detail),'team-stop-reason');reason.title=detail.stopReason;statusLine.append(reason);}
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
    if(this.stalled){
      // stall 顶部提示条（§6.3「20 分钟无成功只通知不停止」）：事件到达展示，
      // 后续任何任务事件推进即由 applyFrame 撤下；sinceMs 为当时已停时长。
      const banner=this.node('div',undefined,'team-stall-banner');
      banner.append(this.node('span',t('超过 20 分钟无进展（通知已发送）')));
      const since=teamDurationText(this.stalled.sinceMs);
      if(since)banner.append(this.node('span',since,'muted'));
      if(this.stalled.lastCompletedTaskId){
        banner.append(this.node('span',t('最近完成：')+this.stalled.lastCompletedTaskId,'muted'));
      }
      body.append(banner);
    }
    const counts=this.meta?.taskCounts;
    if(counts&&typeof counts==='object'){
      const summary=Object.entries(counts).map(([key,value])=>t(teamTaskStatusLabel(key))+' '+value).filter(Boolean).join(' · ');
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
    // 统计卡（阶段四）：进页拉一次，手动刷新按钮重拉；404（旧网关）整块不显示。
    if(this.stats)body.append(this.renderStats());
    // 成员提问（§5.4，detail.pendingQuestions 是权威快照）：open 画应答卡，
    // answered/withdrawn 画灰态徽标——放在任务列表前，等待输入最先被看到。
    const questions=detail.pendingQuestions;
    if(Array.isArray(questions)&&questions.length){
      this.pruneQuestionState(questions);
      body.append(this.renderQuestions(questions));
    }
    // Lead 动作预览（§5.2/§11）：任务列表上方展示三态（proposed/executed/
    // rejected），拒绝原因与派生任务引用从 detail 读（run 级事件不留 payload）。
    const pendingActions=detail.pendingActions;
    if(Array.isArray(pendingActions)&&pendingActions.length)body.append(this.renderPendingActions(pendingActions));
    // 阶段图（§11.3）：Kahn 分层节点卡，点击定位下方任务卡；404/空（旧快照）
    // 回退平铺列表——图是补充视图，缺省时任务卡照常渲染。
    const graphSection=this.renderGraph();
    if(graphSection)body.append(graphSection);
    const tasks=detail.tasks||[];
    if(!tasks.length)body.append(this.node('p',t('还没有任务。'),'muted'));
    for(const task of tasks)body.append(this.renderTask(task));
    root.append(body);
    body.scrollTop=scrollTop;
  }
  /* 合并验证事实：命令读 Run 声明的顶层 verifyCommand（string[]，阶段二遗留
   * 修复——detail.verify 是 VerifyResult（exitCode/outputHead/truncated），没有
   * command 字段，旧代码读 detailVerify.command 恒空）；实时态读随帧
   * meta.verify（verifying/exitCode 摘要）。postAction 后 meta 置空，回落 detail。 */
  verifyInfo(){
    const detail=this.detail;
    const detailVerify=detail&&detail.verify&&typeof detail.verify==='object'?detail.verify:null;
    const metaVerify=this.meta&&this.meta.verify&&typeof this.meta.verify==='object'?this.meta.verify:null;
    const command=detail&&Array.isArray(detail.verifyCommand)?detail.verifyCommand.join(' '):'';
    if(!command&&!detailVerify&&!metaVerify)return null;
    const exitCode=typeof metaVerify?.exitCode==='number'?metaVerify.exitCode
      :(typeof detailVerify?.exitCode==='number'?detailVerify.exitCode:null);
    return {
      command,
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
  /* 「继续」提示按停靠原因分流（与 Runner.Resume/PlanResume 同口径）：预算停靠上调上限
     （服务端额外落 run_budget_resumed）；verify_failed 重跑闸门；quota/auth/not_configured
     停靠按 attempt+1 重派；outcome_unknown 落 stopped 不重发；已失败任务不重试，依赖它的分支终止。 */
  resumeHint(){
    const t=BridgeI18n.t,stopReason=this.detail.stopReason||'',tasks=Array.isArray(this.detail.tasks)?this.detail.tasks:[];
    if(stopReason.startsWith('budget_exhausted'))return t('预算已用尽；继续会提高预算上限并恢复运行。');
    if(stopReason.startsWith('verify_failed'))return t('继续会重新运行验证命令。');
    const parts=[];
    if(tasks.some(task=>task.status==='stopped'&&TEAM_BLOCKED_OUTCOMES.has(task.outcome)))parts.push(t('处理账号或配置后，继续会重新派发停靠的任务。'));
    if(tasks.some(task=>task.status==='outcome_unknown'))parts.push(t('结果未知任务已核查，不再重发；继续可继续的分支。'));
    if(tasks.some(task=>task.status==='failed'))parts.push(t('失败的任务不会重试，依赖它的分支会终止。'));
    return parts.join(' ')||t('继续会派发剩余可运行的任务。');
  }
  renderActions(){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'team-run-actions');
    const status=this.detail.status;
    const stopReason=this.detail.stopReason||'';
    if(status==='waiting_input'){
      const hint=this.resumeHint();
      if(hint)box.append(this.node('span',hint,'muted team-resume-hint'));
      // 验证闸门失败停靠（stopReason 以 verify_failed 开头）时，resume 即重跑闸门。
      const verifyFailed=stopReason.startsWith('verify_failed');
      const resume=this.button(verifyFailed?t('重新验证'):t('继续'),async()=>{
        await this.postAction('resume');
      });
      box.append(resume);
    }
    if(status==='paused'){
      // 暂停态（阶段四）：同一 resume 端点恢复（契约 waiting_input|paused→200）。
      box.append(this.node('span',t('已暂停；继续会从停止处恢复。'),'muted team-resume-hint'));
      box.append(this.button(t('继续'),async()=>{
        await this.postAction('resume');
      }));
    }
    if(!TEAM_TERMINAL_RUN_STATUSES.has(status)){
      // 暂停仅对进行中的 Run 有意义（waiting_input 本就停靠、paused 已停）。
      if(status!=='paused'&&status!=='waiting_input'){
        box.append(this.button(t('暂停'),async()=>{
          await this.postAction('pause');
        }));
      }
      const cancel=this.button(t('取消'),async()=>{
        await this.postAction('cancel');
      });
      box.append(cancel);
    }
    // 修订 Run（§10.2 amend）：从当前 Run detail 预填三段流对话框，任何状态
    // 都可派生修订（典型时点是终态看完结果后决定下一步）。
    box.append(this.button(t('创建修订 Run'),()=>this.openAmend()));
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
      // 暂停/继续/取消会改变图节点状态与统计口径：动作成功后补拉一次补充视图
      //（404 静默回退，旧网关不报错）。
      this.loadGraph();
      this.loadStats();
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
    card.dataset.taskId=task.id; // 动作卡/问题卡的来源任务与派生任务跳转锚点
    const head=this.node('div',undefined,'task-head');
    head.append(this.node('strong',task.id));
    head.append(this.node('span',(member?(member.name||member.id):task.memberId)+(member?' · '+BridgeI18n.agentLabel(member.agentKind):''),'task-member'));
    head.append(this.taskBadge(task.status));
    card.append(head);
    if(task.instructionsHead)card.append(this.node('p',task.instructionsHead,'task-instructions'));
    const metaBits=[];
    if(task.broadcastGroup)metaBits.push(t('并行组：')+task.broadcastGroup);
    if(task.dependsOn&&task.dependsOn.length)metaBits.push(t('依赖：')+task.dependsOn.join(t('、')));
    if(task.writeLock&&task.writeLock!=='none')metaBits.push(t('写锁：')+(task.writeLock==='exclusive'?t('独占'):task.writeLock));
    if(task.baseRevision)metaBits.push(t('基线：')+String(task.baseRevision).slice(0,12));
    if(task.cachedFrom)metaBits.push(t('缓存复用：')+task.cachedFrom);
    if(task.executionThreadId)metaBits.push(t('执行线程：')+task.executionThreadId);
    if(this.detail.workspace?.writeLockHeldBy===task.id)metaBits.push(t('持有项目写锁'));
    if(metaBits.length)card.append(this.node('p',metaBits.join(' · '),'muted task-meta'));
    // 停靠类 outcome 已由下方「停靠原因」按结构化码本地化，服务端整句不再重复展示。
    if(task.stopReason&&task.status!=='completed'&&!TEAM_BLOCKED_OUTCOMES.has(task.outcome))card.append(this.node('p',t('停止原因：')+t(task.stopReason),'muted'));
    // 停靠分类（§6.3 错误策略表）：quota/auth/not_configured 等停靠的任务停在
    // 当前分支并等用户处理，提示原因而不自动重试。
    const outcomeLabel=task.outcome?TEAM_TASK_OUTCOME_LABELS[task.outcome]:'';
    if(outcomeLabel)card.append(this.node('p',t('停靠原因：')+t(outcomeLabel),'muted'));
    if(task.error)card.append(this.node('p',t(task.error),'error'));
    const result=task.result;
    if(result&&typeof result==='object'){
      const box=this.node('div',undefined,'task-result');
      const resultHead=this.node('p',undefined,'task-result-head');
      resultHead.append(this.node('strong',t('结果')));
      const chars=typeof result.resultChars==='number'?result.resultChars:String(result.result||'').length;
      resultHead.append(this.node('span',chars+' '+t('字符'),'muted'));
      if(result.truncated)resultHead.append(this.node('span',t('已截断'),'team-truncated'));
      box.append(resultHead);
      // Lead 结果里的动作信封折叠为卡片（与聊天时间线同一解析），正文仍按原文 pre 展示。
      if(result.result){
        const parts=typeof splitTeamActions==='function'?splitTeamActions(result.result):[{type:'text',text:result.result}];
        for(const part of parts)box.append(part.type==='actions'?renderTeamActionsCard(part,t):this.node('pre',part.text.trim(),'task-result-text'));
      }
      card.append(box);
    }
    const actions=this.node('div',undefined,'task-actions');
    // 原会话跳转的兜底链：结果引用的会话 > 任务所属执行线程的绑定会话 >
    // 成员绑定会话（阶段一执行线程契约：任务经线程派发，线程绑定是跳转的
    // 权威来源；旧网关无线程投影时退回成员会话）。
    const thread=(this.detail?.threads||[]).find(th=>th.id===task.executionThreadId);
    const sessionId=result?.sessionId||(thread?thread.providerSessionId:null)||(member?member.sessionId:null);
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
      const cleanup=this.taskCleanup.get(task.id);
      if(cleanup&&cleanup.done){
        // 工作区已删除：diff 端点随 worktree 消失，查看/清理入口都撤下。
        actions.append(this.node('span',t('工作区已清理'),'muted'));
      }else{
        actions.append(this.button(this.diffOpen.has(task.id)?t('收起改动'):t('查看改动'),()=>this.toggleTaskDiff(task)));
        // 清理是用户显式触发的删除（§6.4 红线：编排器绝不自动清理）；脏区被
        // 服务端拒绝（409）时就地展示原因，改动保留。
        const cleanupButton=this.button(t('清理工作区'),()=>this.cleanupTaskWorkspace(task));
        cleanupButton.disabled=!!(cleanup&&cleanup.busy);
        actions.append(cleanupButton);
      }
    }
    if(actions.childNodes.length)card.append(actions);
    const cleanupState=this.taskCleanup.get(task.id);
    if(cleanupState&&cleanupState.error)card.append(this.node('p',cleanupState.error,'error'));
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
  /* ---- 工作区清理（§6.4）----
     用户显式触发的 worktree 删除（服务端 git worktree remove 不带 --force，
     脏区 409 拒绝且改动保留）；成功后任务快照的 workspaceId/baseRevision 仍在
     （审计），但 diff 与清理入口随 worktree 消失撤下。反馈照 forwardDialog 的
     内联错误风格落在任务卡内。 */
  async cleanupTaskWorkspace(task){
    const t=BridgeI18n.t;
    if(!window.confirm(t('将删除该任务的独立 worktree（有未提交改动时会拒绝并保留）。确认清理？')))return;
    const entry=this.taskCleanup.get(task.id)||{busy:false,error:'',done:false};
    if(entry.busy)return;
    entry.busy=true;entry.error='';
    this.taskCleanup.set(task.id,entry);
    this.render();
    const generation=this.generation;
    try{
      await this.request(this.runUrl('tasks/'+encodeURIComponent(task.id)+'/workspace/cleanup'),{method:'POST'});
      if(generation!==this.generation)return;
      this.taskCleanup.set(task.id,{busy:false,error:'',done:true});
      this.diffOpen.delete(task.id);
      this.taskDiffs.delete(task.id);
      this.render();
    }catch(error){
      if(generation!==this.generation)return;
      this.taskCleanup.set(task.id,{busy:false,error:error.message,done:entry.done});
      this.render();
    }
  }
  /* ---- 成员提问（§5.4 escalate → decision）----
     问题快照在 RunDetail.pendingQuestions（权威，不进 meta）：open 画应答卡
     （summary 等宽、来源任务、版本号小字、选项即答），answered/withdrawn 画
     灰态徽标。提交走 POST runUrl(decision)——绝不能复用 /api/sessions/:id/
     respond（那是会话侧 pendingID 通道，无 questionId+version 版本保护）；
     response 形态按 respondWire 口径：question 类（AskUserQuestion→permission）
     用 {optionId}/{cancel:true}，input 类（elicitation）用 {action,content}。
     409（版本不匹配/已回答/已撤回）提示后整页补读；400（选项无效/格式不正确）
     提示但保持卡可重答——草稿与错误存 questionState，帧重渲染不丢。 */
  renderQuestions(questions){
    const box=this.node('div',undefined,'team-questions');
    for(const question of questions)box.append(this.renderQuestionCard(question));
    return box;
  }
  // questionState 是 Map 字段（构造器），取条目的方法另名 questionEntry。
  questionEntry(question){
    const key=String(question.questionId||'');
    let entry=this.questionState.get(key);
    if(!entry){entry={busy:false,error:'',draft:''};this.questionState.set(key,entry);}
    return entry;
  }
  // 只保留当前快照里仍存在的问题的本地态：撤走的问题连同错误/草稿一并丢弃。
  pruneQuestionState(questions){
    const live=new Set();
    for(const question of questions)if(question.questionId)live.add(String(question.questionId));
    for(const key of this.questionState.keys())if(!live.has(key))this.questionState.delete(key);
  }
  questionStatusBadge(status){
    // 「问题已撤回」不复用聊天队列的「已撤回」键（那里指排队消息撤回，英文
    // 语义不同）；问题态用独立词条。
    return teamStatusBadge('stopped',{stopped:status==='answered'?'已回答':status==='withdrawn'?'问题已撤回':status});
  }
  renderQuestionCard(question){
    const t=BridgeI18n.t;
    const status=question.status||'open';
    const open=status==='open';
    // open 用 .approval 卡（与聊天页待确认卡同款琥珀底=需要你处理）；已解决
    // 的退成白卡+灰态徽标，靠后不抢注意力。
    const card=this.node('section',undefined,open?'approval':'task-card');
    if(open){
      const head=this.node('h3');
      head.append(document.createTextNode(t('成员提问')));
      card.append(head);
    }else{
      const head=this.node('div',undefined,'task-head');
      head.append(this.questionStatusBadge(status));
      card.append(head);
    }
    if(question.summary)card.append(this.node('pre',question.summary,open?undefined:'task-result-text'));
    const bits=[];
    if(question.taskId)bits.push(t('来源任务：')+question.taskId);
    if(typeof question.version==='number')bits.push(t('版本：')+question.version);
    if(bits.length)card.append(this.node('p',bits.join(' · '),open?'muted':'task-meta'));
    if(!open)return card;
    const state=this.questionEntry(question);
    const options=Array.isArray(question.options)?question.options:[];
    const actionsBox=this.node('div',undefined,'actions');
    const cancelResponse=question.kind==='input'?{action:'cancel'}:{cancel:true};
    const cancel=this.button(t('取消'));
    cancel.className='secondary';cancel.disabled=state.busy;
    cancel.onclick=()=>this.submitDecision(question,cancelResponse);
    if(options.length){
      // 选项即答（app.js renderAcpApproval 的 question 卡形态）：每个选项一个
      // 按钮直接提交 {optionId}，无独立「提交」步。
      for(const option of options){
        const optionButton=this.button(option.label||option.id||'');
        optionButton.className='secondary';
        optionButton.disabled=state.busy;
        optionButton.onclick=()=>this.submitDecision(question,{optionId:option.id});
        actionsBox.append(optionButton);
      }
    }else if(question.kind==='input'){
      const label=this.node('label');
      const input=document.createElement('textarea');
      input.rows=3;input.maxLength=100000;
      input.value=state.draft;
      input.oninput=()=>{state.draft=input.value;};
      label.append(input);
      card.append(label);
      const submit=this.button(t('提交回答'));
      submit.className='primary';submit.disabled=state.busy;
      submit.onclick=()=>{
        if(!state.draft.trim()){state.error=t('请输入回复内容。');this.render();return;}
        this.submitDecision(question,{action:'accept',content:state.draft});
      };
      actionsBox.append(submit);
    }
    // question 形态却无选项（服务端契约下不该出现）：只保留取消，卡不失效。
    actionsBox.append(cancel);
    card.append(actionsBox);
    if(state.error)card.append(this.node('p',state.error,'error'));
    return card;
  }
  submitDecision(question,response){
    const state=this.questionEntry(question);
    if(state.busy||!question.questionId)return;
    state.busy=true;state.error='';
    this.render();
    const generation=this.generation;
    // 返回 promise 链（自带 catch 不外抛）：调用方可等待，测试可断言落点。
    return this.request(this.runUrl('decision'),{method:'POST',body:{
      // decisionId 每次点击新生成：答案只允许消费一次（§5.4），重复/过期提交
      // 由服务端按问题状态拒绝（409），客户端幂等键不承担去重职责。
      decisionId:teamUUID(),
      questionId:question.questionId,
      version:question.version,
      response,
    }}).then(()=>{
      if(generation!==this.generation)return;
      // decision 响应是 {ok,duplicate?} 而非 RunDetail：不能走 postAction 的整
      // 替换，手动补读刷新问题态（busy 保持到补读落地，防止双击重复作答）。
      this.scheduleRefetch(0);
    }).catch(error=>{
      if(generation!==this.generation)return;
      state.busy=false;state.error=error.message;
      this.render();
      // 409=版本不匹配/已回答/已撤回：本地快照已过期，整页补读拿权威态。
      if(error.status===409)this.scheduleRefetch(0);
    });
  }
  /* ---- Lead 动作预览（§5.2/§11）----
     每条：type 徽标、目标成员、轮次、task 摘要、来源任务（可跳转）；状态三态
     ——proposed 在 autoDispatch 关闭的 Team 标注「仅预览」，executed 给派生任务
     跳转，rejected 醒目展示 reason。全部字段从 detail.pendingActions 读。 */
  renderPendingActions(actions){
    const t=BridgeI18n.t;
    const box=this.node('div',undefined,'team-pending-actions');
    box.append(this.node('p',t('Lead 动作'),'muted'));
    for(const action of actions)box.append(this.renderActionCard(action));
    return box;
  }
  renderActionCard(action){
    const t=BridgeI18n.t;
    const card=this.node('section',undefined,'task-card');
    const head=this.node('div',undefined,'task-head');
    head.append(teamStatusBadge(action.type||'',TEAM_ACTION_TYPE_LABELS));
    const statusBadge=TEAM_ACTION_STATUS_BADGES[action.status];
    if(statusBadge)head.append(teamStatusBadge(statusBadge.cls,{[statusBadge.cls]:statusBadge.label}));
    else if(action.status)head.append(teamStatusBadge(action.status,{[action.status]:action.status}));
    if(action.member){
      const member=this.teamMember(action.member);
      head.append(this.node('span',member?(member.name||member.id):action.member,'task-member'));
    }
    if(action.round)head.append(this.node('span',t('第 ')+action.round+t(' 轮'),'task-member'));
    card.append(head);
    if(action.task)card.append(this.node('p',action.task,'task-instructions'));
    const actionsBox=this.node('div',undefined,'task-actions');
    if(action.sourceTaskId){
      if(this.findTask(action.sourceTaskId)){
        actionsBox.append(this.button(t('来源任务：')+action.sourceTaskId,()=>this.revealTask(action.sourceTaskId)));
      }else{
        actionsBox.append(this.node('span',t('来源任务：')+action.sourceTaskId,'muted'));
      }
    }
    if(action.status==='executed'){
      const derived=this.derivedTaskFor(action);
      if(derived)actionsBox.append(this.button(t('派生任务：')+derived.id,()=>this.revealTask(derived.id)));
    }
    if(actionsBox.childNodes.length)card.append(actionsBox);
    if(action.status==='proposed'&&this.team?.policy?.autoDispatch!==true){
      card.append(this.node('p',t('未开启自动执行，仅预览。'),'muted'));
    }
    if(action.status==='rejected'&&action.reason){
      card.append(this.node('p',t('拒绝原因：')+action.reason,'error'));
    }
    return card;
  }
  findTask(taskId){
    return ((this.detail&&this.detail.tasks)||[]).find(task=>task.id===taskId)||null;
  }
  /* executed 动作的派生任务定位：actionKey 是动作幂等键（team:{t}:run:{r}:
     action:{sourceTaskID}:{round}:{index}），执行流的 task_planned 事件载荷携带
     同键（store 重建据此标 executed）——按 applyEvent 记账的 taskId→actionKey
     映射反查派生任务。fromTaskId 是 request_context 的结果来源任务（非派生
     任务，model.go Action 注释），不作派生链接；映射缺失（事件被环形缓冲裁
     剪）时只显示徽标不硬造链接。 */
  derivedTaskFor(action){
    if(!action.actionKey)return null;
    const tasks=(this.detail&&this.detail.tasks)||[];
    return tasks.find(task=>this.taskActionKeys.get(task.id)===action.actionKey)||null;
  }
  revealTask(taskId){
    const selector=typeof CSS!=='undefined'&&CSS.escape
      ?'.task-card[data-task-id="'+CSS.escape(taskId)+'"]'
      :'.task-card[data-task-id="'+taskId+'"]';
    const target=this.root.querySelector(selector);
    if(!target)return;
    target.scrollIntoView({block:'center',behavior:'smooth'});
    // 就地高亮 1.6s（帧重渲染可能换掉节点，超时回调摸到的是游离节点，无害）。
    target.style.outline='2px solid #478767';
    setTimeout(()=>{target.style.outline='';},1600);
  }
  /* ---- 修订 Run（§10.2/§6.4.1 amend 缓存）----
     从当前 Run detail 预填三段流对话框（teamRunDialogHost 复用）：goal/mode/
     collectPolicy/verifyCommand 原样带回；任务行由 detail.tasks 反填——
     broadcast 的逐成员展开任务按 broadcastGroup 聚合回条目行（展开 ID 是
     {entryID}-{memberID}，依赖映射回调目 ID），workspaceMode/workspaceId 透传。
     提交打 POST runUrl(amend)，幂等键沿用对话框 requestId 机制（失败重试同 id
     重放，返回编辑重生成）；成功 openRun 新 Run 并提示 cacheImports 摘要。 */
  async openAmend(){
    const t=BridgeI18n.t;
    const detail=this.detail;
    if(!detail)return;
    if(!this.team){
      // openRun 时团队读取失败不阻塞展示；修订需要成员映射，这里补读一次。
      try{this.team=await this.request('/api/teams/'+encodeURIComponent(this.teamId));}
      catch{this.team=null;}
    }
    if(!this.team){
      this.actionError=t('团队信息不可用，无法创建修订 Run。');
      this.render();
      return;
    }
    const mode=['single','pipeline','broadcast'].includes(detail.mode)?detail.mode:'single';
    const tasks=mode==='single'?[]
      :mode==='broadcast'?this.amendBroadcastRows(detail)
      :this.amendPipelineRows(detail);
    const teamId=this.teamId;
    teamRunDialogHost.open({
      team:this.team,
      amend:{runId:this.runId},
      goal:detail.goal||'',
      mode,
      collect:detail.collectPolicy||'all',
      tasks,
      verifyText:Array.isArray(detail.verifyCommand)?detail.verifyCommand.join(' '):'',
      submit:payload=>this.request(this.runUrl('amend'),{method:'POST',body:payload}),
      onSuccess:run=>{
        // 响应是新 Run 的 RunDetail + parentRunId/cacheImports；openRun 的同步
        // 段会清 actionNotice，此处紧随其后赋值，晚到的首帧 render 仍会带上。
        this.openRun(teamId,run.id);
        const imports=Array.isArray(run.cacheImports)?run.cacheImports.length:0;
        if(imports)this.actionNotice=t('已创建修订 Run；')+imports+t(' 个任务命中缓存，未重复执行。');
      },
    });
  }
  amendPipelineRows(detail){
    return (detail.tasks||[]).map(task=>({
      id:task.id,
      memberId:task.memberId||'',
      memberIds:new Set(),
      deps:new Set(task.dependsOn||[]),
      prompt:task.prompt||'',
      writeLock:task.writeLock||'none',
      workspaceMode:task.workspaceMode||'',
      workspaceId:task.workspaceId||'',
    }));
  }
  amendBroadcastRows(detail){
    const rows=new Map();    // broadcastGroup（条目 ID）→ 聚合行
    const groupOf=new Map(); // 展开任务 ID → broadcastGroup（依赖回调目用）
    for(const task of detail.tasks||[])if(task.broadcastGroup)groupOf.set(task.id,task.broadcastGroup);
    for(const task of detail.tasks||[]){
      const groupId=task.broadcastGroup||task.id;
      let row=rows.get(groupId);
      if(!row){
        row={id:groupId,memberId:'',memberIds:new Set(),deps:new Set(),
          prompt:task.prompt||'',writeLock:task.writeLock||'none',
          workspaceMode:task.workspaceMode||'',workspaceId:task.workspaceId||''};
        rows.set(groupId,row);
      }
      if(task.memberId)row.memberIds.add(task.memberId);
      for(const dep of task.dependsOn||[]){
        const depGroup=groupOf.get(dep)||dep;
        if(depGroup!==groupId)row.deps.add(depGroup); // 组内互依赖不回填
      }
    }
    return [...rows.values()];
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
    // 来源会话兜底链与任务卡的原会话跳转一致（阶段一执行线程契约）：
    // 结果引用 > 执行线程绑定 > 成员绑定。
    const thread=(this.detail?.threads||[]).find(th=>th.id===task.executionThreadId);
    dialog.forwardState={
      taskId:task.id,
      sourceSessionId:task.result?.sessionId||(thread?thread.providerSessionId:null)||(member?member.sessionId:null),
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
if(typeof module!=='undefined')module.exports={TeamsPanel,TeamRunScreen,TeamRunDialogHost,teamRunStopReasonText};
