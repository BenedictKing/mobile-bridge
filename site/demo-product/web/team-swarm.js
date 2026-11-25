'use strict';

// Swarm 只展示服务端的契约、候选与评测事实；客户端不推断目标已达成。
const TEAM_SWARM_EN={
  '目标契约与评测':'Goal contract and evaluations','原始要求':'Original requirement',
  '契约待定义':'Contract not defined','契约待接受':'Contract awaiting acceptance','契约已冻结':'Contract frozen',
  '当前版本不支持此契约，请更新客户端。':'Update the client to use this contract version.',
  '还没有评测证据。':'No evaluation evidence yet.','还没有候选产物。':'No candidate artifacts yet.',
  '补充人工确认条件（每行一项）':'Additional human criteria (one per line)',
  '机器验收条件（每行一项）':'Machine-checkable criteria (one per line)',
  '检查命令（每行一个参数）':'Check command (one argument per line)',
  '直接执行参数，不支持管道、重定向或 shell 表达式。':'Arguments are executed directly. Pipes, redirects and shell expressions are not supported.',
  '允许修改路径（每行一项）':'Allowed write paths (one per line)',
  '检查工作区 ID（可选）':'Evaluation workspace ID (optional)',
  '任务范围':'Task scope','全部执行任务':'All execution tasks',
  '每个条件的修复上限':'Repair limit per criterion','评测次数上限':'Evaluation limit',
  '检查超时（秒）':'Check timeout (seconds)','必须经过独立审查':'Require independent review',
  '提出验收契约':'Propose acceptance contract','接受并冻结契约':'Accept and freeze contract',
  '冻结后按此版本检查；这不会合入或发布文件。':'Checks use the frozen version. Accepting does not merge or publish files.',
  '请同时填写机器验收条件和检查命令。':'Provide both machine criteria and a check command.',
  '允许修改路径不能为空。':'Allowed write paths cannot be empty.',
  '请输入有效的预算和超时。':'Enter valid budgets and timeout.',
  '检查命令不支持 shell 语法。':'Shell syntax is not supported in check commands.',
  '检查命令最多 8 个参数，每个参数最多 512 字节。':'Use at most 8 command arguments and 512 bytes per argument.',
  '条件最多 20 项，包含原始要求。':'Use at most 20 criteria, including the original requirement.',
  '保存中…':'Saving…','正在检查…':'Evaluating…','正在集成…':'Integrating…','操作已记录。':'Operation recorded.',
  '操作已记录，但刷新失败；请刷新查看最新状态。':'The operation was recorded, but refreshing failed. Refresh to see its status.',
  '版本已变化，请刷新后再操作。':'The version changed. Refresh before trying again.',
  '刷新证据':'Refresh evidence','刷新中…':'Refreshing…','操作失败':'Operation failed',
  '完成条件':'Completion criteria','人工确认':'Human confirmation','机器检查':'Machine check','独立审查':'Independent review',
  '必须通过':'Required','辅助检查':'Advisory','待确认':'Needs confirmation','满足':'Met','不满足':'Unmet',
  '评测记录':'Evaluations','候选产物':'Candidate artifacts','集成候选':'Integration candidate',
  '已通过':'Passed','未通过':'Failed','未取得结论':'Inconclusive','已中断':'Interrupted',
  '待执行':'Pending','运行中':'Running','已过期':'Stale','有效':'Fresh','有效性未知':'Validity unknown',
  '证据详情':'Evidence details','输出已截断':'Output truncated','退出码':'Exit code',
  '复验此检查':'Recheck','运行候选检查':'Evaluate candidate','选择检查':'Choose a check','选择已完成任务':'Choose a completed task',
  '没有可检查的已完成任务。':'No completed task is available for evaluation.',
  '当前契约没有配置机器检查。':'No machine checks are configured for this contract.',
  '选择要集成的候选':'Select candidates to integrate','集成所选候选':'Integrate selected candidates','选择检查对象':'Choose an evaluation subject',
  '至少选择两个候选；集成后仍须整体验证。':'Select at least two candidates. The integrated result still needs verification.',
  '待处理问题':'Findings','修复次数':'Repairs','评测次数':'Evaluations','未知':'Unknown',
  '本地快照检查，不等同于操作系统沙箱。':'Local snapshot checks do not provide an operating-system sandbox.',
  '证据过期或有效性未知，不能用于当前验收。':'Stale or unknown-validity evidence cannot satisfy current acceptance criteria.',
  '未记录成本':'Cost not recorded','检查规格':'Evaluation specifications','任务边界':'Task boundaries',
  '暂停或取消期间不能发起新检查。':'New checks are unavailable while the run is paused or cancelled.',
  '契约草稿':'Contract draft','查看契约配置':'View contract configuration','未取得独立审查结论':'No independent review verdict yet',
  '契约尚未冻结，不能运行候选检查。':'Freeze the contract before evaluating candidates.',
  '等待处理':'Needs attention','待验证':'Needs verification','结果待确认':'Result needs confirmation',
  '非独立审查':'Non-independent review','审查':'Review','检查对象尚未就绪。':'The evaluation subject is not ready.',
  'outcome_unknown':'Result needs reconciliation',
  '查看候选差异':'View candidate diff','正在读取差异…':'Loading diff…','读取差异失败，可重试。':'Could not load the diff. Try again.',
  '重试读取差异':'Retry loading diff','此候选没有文件差异。':'This candidate has no file changes.',
  '差异已截断，仅供查看，不能据此完整应用。':'The diff is truncated. It is for inspection and cannot be used to apply the complete candidate.',
};

function teamSwarmText(value){
  const source=String(value??'');
  const i18n=typeof BridgeI18n!=='undefined'?BridgeI18n:null;
  const translated=i18n?.t?i18n.t(source):(typeof t_safe==='function'?t_safe(source):source);
  if(translated!==source)return translated;
  return i18n?.language?.()==='en'?(TEAM_SWARM_EN[source]||source):source;
}

function teamSwarmUUID(){
  if(typeof crypto!=='undefined'&&crypto.randomUUID)return crypto.randomUUID();
  const bytes=new Uint8Array(16);
  if(typeof crypto!=='undefined'&&crypto.getRandomValues)crypto.getRandomValues(bytes);
  else for(let i=0;i<bytes.length;i++)bytes[i]=Math.floor(Math.random()*256);
  bytes[6]=(bytes[6]&15)|64;bytes[8]=(bytes[8]&63)|128;
  const hex=Array.from(bytes,x=>x.toString(16).padStart(2,'0')).join('');
  return [hex.slice(0,8),hex.slice(8,12),hex.slice(12,16),hex.slice(16,20),hex.slice(20)].join('-');
}

function teamSwarmLines(value){return String(value||'').split('\n').map(x=>x.trim()).filter(Boolean);}

// 一行一个 argv 项，保留含空格的单个参数，不猜测 shell 引号规则。
function teamSwarmCommand(value){
  const argv=teamSwarmLines(value);
  if(argv.some(x=>/[|;&$`><\r]/.test(x)))throw new Error(teamSwarmText('检查命令不支持 shell 语法。'));
  if(argv.length>8||argv.some(x=>new TextEncoder().encode(x).length>512))throw new Error(teamSwarmText('检查命令最多 8 个参数，每个参数最多 512 字节。'));
  return argv;
}

function teamSwarmBuildContract(run,draft){
  const machine=teamSwarmLines(draft.machine),human=teamSwarmLines(draft.human),command=teamSwarmCommand(draft.command);
  if(!!machine.length!==!!command.length)throw new Error(teamSwarmText('请同时填写机器验收条件和检查命令。'));
  if(1+machine.length+human.length>20)throw new Error(teamSwarmText('条件最多 20 项，包含原始要求。'));
  const allowedWritePaths=teamSwarmLines(draft.paths);
  if(!allowedWritePaths.length)throw new Error(teamSwarmText('允许修改路径不能为空。'));
  const maxRepairs=Number(draft.maxRepairs),maxEvals=Number(draft.maxEvals),timeoutSeconds=Number(draft.timeoutSeconds);
  if(!Number.isInteger(maxRepairs)||maxRepairs<0||maxRepairs>2||!Number.isInteger(maxEvals)||maxEvals<1||maxEvals>64||!Number.isInteger(timeoutSeconds)||timeoutSeconds<1||timeoutSeconds>600)throw new Error(teamSwarmText('请输入有效的预算和超时。'));
  const criteria=[{id:'original',text:String(run.goal||''),checkKind:'human',required:true}];
  human.forEach((text,i)=>criteria.push({id:'human-'+(i+1),text,checkKind:'human',required:true}));
  machine.forEach((text,i)=>criteria.push({id:'check-'+(i+1),text,checkKind:'deterministic',required:true}));
  const evalSpecs=command.length?[{id:'acceptance',criterionIds:criteria.filter(x=>x.checkKind==='deterministic').map(x=>x.id),command,timeoutSeconds}]:[];
  if(evalSpecs.length&&draft.workspaceId?.trim())evalSpecs[0].workspaceId=draft.workspaceId.trim();
  return {criteria,evalSpecs,tasks:[{taskId:draft.taskId||'*',criterionIds:criteria.map(x=>x.id),allowedWritePaths,evalSpecIds:evalSpecs.map(x=>x.id)}],requireIndependentReview:!!draft.requireIndependentReview,maxRepairs,maxEvals};
}

const TEAM_SWARM_STATUS={
  passed:'已通过',failed:'未通过',inconclusive:'未取得结论',interrupted:'已中断',
  pending:'待执行',requested:'待执行',running:'运行中',started:'运行中',
  stale:'已过期',fresh:'有效',unknown:'未知',met:'满足',unmet:'不满足',
  proposed:'契约待接受',awaiting_contract:'契约待定义',accepted:'契约已冻结',
  waiting_input:'等待处理',pending_verification:'待验证',pending_confirmation:'结果待确认',
};

class TeamSwarmPanel{
  constructor({run,request,onChange,drafts,teamId}){
    this.run=run||{};this.request=request;this.onChange=onChange;this.teamId=teamId||run?.teamId;
    this.drafts=drafts||new Map();
    const key=String(this.teamId)+'|'+String(this.run.id);
    if(!this.drafts.has(key))this.drafts.set(key,{
      human:'',machine:'',command:'',paths:'.',workspaceId:'',taskId:'*',
      maxRepairs:'2',maxEvals:'12',timeoutSeconds:'600',requireIndependentReview:true,
      busy:false,error:'',notice:'',operations:new Map(),selectedArtifacts:new Set(),open:new Set(),
    });
    this.state=this.drafts.get(key);this.state.artifactDiffs??=new Map();this.controls=[];
    this.root=this.node('section',undefined,'team-goal-review team-swarm-panel');
    this.root.dataset.runId=String(this.run.id||'');
  }
  node(tag,text,cls){const el=document.createElement(tag);if(text!==undefined)el.textContent=text;if(cls)el.className=cls;return el;}
  text(label){return teamSwarmText(label);}
  url(action){return '/api/teams/'+encodeURIComponent(this.teamId||'')+'/runs/'+encodeURIComponent(this.run.id||'')+(action?'/'+action:'');}
  control(el,blocked=false){el._swarmBlocked=blocked;el.disabled=!!this.state.busy||blocked;this.controls.push(el);return el;}
  button(label,action,blocked=false){const el=this.node('button',this.text(label));el.type='button';el.addEventListener('click',action);return this.control(el,blocked);}
  field(label,control){const el=this.node('label',undefined,'team-goal-field');el.append(this.node('span',this.text(label)),control);return el;}
  textarea(name,rows=2){const el=this.node('textarea');el.name=name;el.rows=rows;el.value=this.state[name]||'';el.style.maxWidth='100%';el.addEventListener('input',()=>{this.state[name]=el.value;});return this.control(el);}
  number(name,min,max){const el=this.node('input');el.name=name;el.type='number';el.min=String(min);el.max=String(max);el.value=this.state[name];el.addEventListener('input',()=>{this.state[name]=el.value;});return this.control(el);}
  select(name,options,value,update){const el=this.node('select');el.name=name;for(const [id,label] of options){const option=this.node('option',label);option.value=id;el.append(option);}el.value=value;el.addEventListener('change',()=>update(el.value));return this.control(el);}
  details(key,label){const el=this.node('details');el.open=this.state.open.has(key);el.append(this.node('summary',this.text(label)));el.addEventListener('toggle',()=>{if(el.open)this.state.open.add(key);else this.state.open.delete(key);});return el;}
  status(value){return this.text(TEAM_SWARM_STATUS[value]||value||'未知');}
  badge(value){const cls=value==='passed'||value==='fresh'||value==='met'?'b-completed':value==='failed'||value==='unmet'?'b-failed':'b-waiting_input';return this.node('span',this.status(value),'team-badge '+cls);}
  get swarm(){return this.run.swarm||{};}
  get proposal(){return this.swarm.proposal||(this.swarm.proposals||[]).at(-1)||null;}
  get contract(){return this.swarm.contract||null;}
  get blocked(){return ['paused','cancelled','outcome_unknown'].includes(this.run.status);}
  sync(){for(const el of this.controls)el.disabled=!!this.state.busy||el._swarmBlocked;this.root.setAttribute('aria-busy',String(!!this.state.busy));if(this.error)this.error.textContent=this.state.error;if(this.notice)this.notice.textContent=this.state.notice;}
  absorb(result){
    if(result?.run?.id===this.run.id)this.run=result.run;
    else if(result?.id===this.run.id&&result?.teamId)this.run=result;
    else if(result?.swarm)this.run={...this.run,swarm:result.swarm,version:result.version??this.run.version};
    else if(result?.schemaVersion)this.run={...this.run,swarm:result,version:result.runVersion??this.run.version};
  }
  async refresh(){
    if(this.state.busy)return;
    this.state.busy=true;this.state.error='';this.state.notice=this.text('刷新中…');this.sync();
    try{const result=await this.request(this.url('swarm'));this.absorb(result);this.state.notice='';await this.onChange?.(result);}
    catch(error){this.state.error=error.message||this.text('操作失败');this.state.notice='';}
    finally{this.state.busy=false;this.render();}
  }
  async act(action,payload={},notice='保存中…'){
    if(this.state.busy)return;
    const body={version:this.run.version,schemaVersion:1,...payload};
    const signature=JSON.stringify([action,body]);
    if(!this.state.operations.has(signature))this.state.operations.set(signature,teamSwarmUUID());
    body.id=this.state.operations.get(signature);
    this.state.busy=true;this.state.error='';this.state.notice=this.text(notice);this.sync();
    let result;
    try{result=await this.request(this.url(action),{method:'POST',body});}
    catch(error){this.state.error=error.status===409?this.text('版本已变化，请刷新后再操作。')+' '+(error.message||''):error.message||this.text('操作失败');this.state.notice='';this.state.busy=false;this.sync();return;}
    this.absorb(result);this.state.notice=this.text('操作已记录。');
    // 写入已成功时补读失败也不能重新发起该动作。
    try{await this.onChange?.(result);}
    catch(_){this.state.notice=this.text('操作已记录，但刷新失败；请刷新查看最新状态。');}
    finally{this.state.busy=false;this.render();}
    return result;
  }
  propose(){
    if(this.state.busy)return;
    try{return this.act('contract-proposals',{contract:teamSwarmBuildContract(this.run,this.state)});}
    catch(error){this.state.error=error.message;this.sync();}
  }
  async loadArtifactDiff(artifact){
    const key=artifact.id+'|'+(artifact.contentHash||artifact.hash||'');
    const previous=this.state.artifactDiffs.get(key);
    if(previous?.loading||previous?.data)return;
    const entry={loading:true,error:'',data:null};this.state.artifactDiffs.set(key,entry);
    this.render();
    try{
      const result=await this.request(this.url('artifacts/'+encodeURIComponent(artifact.id)+'/diff'));
      if(!result||result.artifactId!==artifact.id||typeof result.diff!=='string')throw new Error(this.text('读取差异失败，可重试。'));
      entry.data=result;
    }catch(error){entry.error=error.message||this.text('读取差异失败，可重试。');}
    finally{entry.loading=false;this.render();}
  }
  renderArtifactDiff(artifact){
    const key=artifact.id+'|'+(artifact.contentHash||artifact.hash||'');
    const details=this.details('artifact-diff-'+key,'查看候选差异');details.dataset.artifactDiffId=artifact.id;
    details.addEventListener('toggle',()=>{
      if(details.open){this.state.open.add('artifact-diff-'+key);if(!this.state.artifactDiffs.has(key))this.loadArtifactDiff(artifact);}
      else this.state.open.delete('artifact-diff-'+key);
    });
    const entry=this.state.artifactDiffs.get(key);
    if(entry?.loading){const notice=this.node('p',this.text('正在读取差异…'),'muted');notice.setAttribute('role','status');details.append(notice);}
    else if(entry?.error){
      const error=this.node('p',entry.error,'error');error.setAttribute('role','alert');details.append(error);
      const actions=this.node('div',undefined,'team-run-actions');actions.append(this.button('重试读取差异',()=>this.loadArtifactDiff(artifact)));details.append(actions);
    }else if(entry?.data){
      const data=entry.data;
      if(data.truncated)details.append(this.node('p',this.text('差异已截断，仅供查看，不能据此完整应用。'),'team-stop-reason'));
      details.append(data.diff?this.node('pre',data.diff):this.node('p',this.text('此候选没有文件差异。'),'muted'));
    }
    return details;
  }
  render(){
    this.controls=[];this.root.replaceChildren();
    this.root.append(this.node('h3',this.text('目标契约与评测')));
    const actions=this.node('div',undefined,'team-run-actions');actions.append(this.button('刷新证据',()=>this.refresh()));
    this.error=this.node('p',this.state.error,'error');this.error.setAttribute('role','alert');
    this.notice=this.node('p',this.state.notice,'muted');this.notice.setAttribute('role','status');
    if(this.swarm.schemaVersion&&this.swarm.schemaVersion!==1){this.root.append(this.node('p',this.text('当前版本不支持此契约，请更新客户端。'),'error'),actions,this.error,this.notice);this.sync();return this.root;}
    const original=this.details('original','原始要求');original.append(this.node('p',this.run.goal||''));this.root.append(original);
    this.root.append(this.node('p',this.text(this.contract?'契约已冻结':this.proposal?'契约待接受':'契约待定义'),'muted'));
    if(this.swarm.reason)this.root.append(this.node('p',this.swarm.reason,'team-stop-reason'));
    if(this.blocked)this.root.append(this.node('p',this.text('暂停或取消期间不能发起新检查。'),'muted'));
    const proposal=this.proposal;
    if(this.contract)this.root.append(this.renderContract(this.contract));
    else if(proposal?.contract){
      this.root.append(this.renderContract(proposal.contract));
      const accept=this.button('接受并冻结契约',()=>this.act('contract-proposals/'+encodeURIComponent(proposal.id)+'/accept'),!proposal.id||this.blocked);
      accept.className='primary';actions.prepend(accept);
      this.root.append(this.node('p',this.text('冻结后按此版本检查；这不会合入或发布文件。'),'muted'));
    }else this.root.append(this.renderForm());
    if(this.contract){this.root.append(this.renderEvaluate(),this.renderArtifacts(),this.renderEvals(),this.renderFindings());this.renderBudget();}
    this.root.append(actions,this.notice,this.error);this.sync();return this.root;
  }
  renderForm(){
    const form=this.node('form');form.addEventListener('submit',event=>{event.preventDefault();this.propose();});
    form.append(this.field('补充人工确认条件（每行一项）',this.textarea('human')),
      this.field('机器验收条件（每行一项）',this.textarea('machine')),
      this.field('检查命令（每行一个参数）',this.textarea('command',3)),
      this.node('p',this.text('直接执行参数，不支持管道、重定向或 shell 表达式。'),'muted'));
    const advanced=this.details('draft-options','查看契约配置');
    const tasks=[['*',this.text('全部执行任务')],...(this.run.tasks||[]).map(t=>[t.id,t.id])];
    advanced.append(this.field('任务范围',this.select('taskId',tasks,this.state.taskId,value=>this.state.taskId=value)),
      this.field('允许修改路径（每行一项）',this.textarea('paths')),
      this.field('检查工作区 ID（可选）',this.textarea('workspaceId',1)),
      this.field('检查超时（秒）',this.number('timeoutSeconds',1,600)),
      this.field('每个条件的修复上限',this.number('maxRepairs',0,2)),
      this.field('评测次数上限',this.number('maxEvals',1,64)));
    const review=this.node('input');review.type='checkbox';review.name='requireIndependentReview';review.checked=this.state.requireIndependentReview;review.addEventListener('change',()=>{this.state.requireIndependentReview=review.checked;});
    advanced.append(this.field('必须经过独立审查',this.control(review)));
    const actions=this.node('div',undefined,'team-run-actions'),submit=this.button('提出验收契约',()=>this.propose(),this.blocked);submit.className='primary';actions.append(submit);form.append(advanced,actions);return form;
  }
  renderContract(contract){
    const box=this.node('div',undefined,'team-swarm-contract');box.append(this.node('h4',this.text('完成条件')));
    const list=this.node('ul');
    for(const criterion of contract.criteria||[]){
      const row=this.node('li');row.dataset.criterionId=criterion.id;
      const type={human:'人工确认',deterministic:'机器检查',review:'独立审查'}[criterion.checkKind]||'未知';
      row.append(this.node('p',criterion.text||''),this.node('small',this.text(type)+' · '+this.text(criterion.required===false?'辅助检查':'必须通过'),'muted'));
      // 缺省保持待确认，不能把 eval 通过直接换成目标 met。
      row.append(this.badge(criterion.state||'unknown'));list.append(row);
    }
    box.append(list);
    if(contract.requireIndependentReview)box.append(this.node('p',this.text('必须经过独立审查'),'muted'));
    const details=this.details('contract-specs','检查规格');
    for(const spec of contract.evalSpecs||[]){
      const row=this.node('div');row.append(this.node('strong',spec.id),this.node('pre',(spec.command||[]).join('\n')));details.append(row);
    }
    for(const task of contract.tasks||[])details.append(this.node('p',this.text('任务边界')+': '+task.taskId+' · '+(task.allowedWritePaths||[]).join(', '),'muted'));
    box.append(details);return box;
  }
  renderEvaluate(){
    const box=this.node('div',undefined,'team-swarm-evaluate');
    const specs=this.contract.evalSpecs||[],tasks=(this.run.tasks||[]).filter(t=>t.status==='completed'&&!t.reduction&&!t.issueClassificationOnly&&t.review!==true);
    if(!specs.length){box.append(this.node('p',this.text('当前契约没有配置机器检查。'),'muted'));return box;}
    const artifacts=(this.swarm.artifacts||[]).filter(x=>x.id);
    const subjects=[...tasks.map(t=>['task:'+t.id,t.id]),...artifacts.map(a=>['artifact:'+a.id,(a.kind==='integration'||a.integration?this.text('集成候选')+' · ':'')+a.id])];
    if(!subjects.length){box.append(this.node('p',this.text('没有可检查的已完成任务。'),'muted'));return box;}
    if(!subjects.some(([id])=>id===this.state.evalSubject))this.state.evalSubject=subjects[0][0];
    if(!specs.some(s=>s.id===this.state.evalSpec))this.state.evalSpec=specs[0].id;
    box.append(this.field('选择检查对象',this.select('evalSubject',subjects,this.state.evalSubject,value=>this.state.evalSubject=value)),
      this.field('选择检查',this.select('evalSpec',specs.map(s=>[s.id,s.id]),this.state.evalSpec,value=>this.state.evalSpec=value)));
    const actions=this.node('div',undefined,'team-run-actions');actions.append(this.button('运行候选检查',()=>{
      const [kind,...parts]=this.state.evalSubject.split(':');
      return this.act('swarm/evaluate',{[kind==='artifact'?'artifactId':'taskId']:parts.join(':'),evalSpecId:this.state.evalSpec},'正在检查…');
    },this.blocked));box.append(actions);return box;
  }
  renderArtifacts(){
    const box=this.details('artifacts','候选产物'),artifacts=this.swarm.artifacts||[];
    if(!artifacts.length){box.append(this.node('p',this.text('还没有候选产物。'),'muted'));return box;}
    const available=new Set(artifacts.map(x=>x.id));
    for(const id of this.state.selectedArtifacts)if(!available.has(id))this.state.selectedArtifacts.delete(id);
    const candidates=artifacts.filter(x=>x.kind!=='integration'&&!x.integration);
    for(const artifact of artifacts){
      const row=this.node('div');row.dataset.artifactId=artifact.id;
      const integration=artifact.kind==='integration'||artifact.integration;
      row.append(this.node('p',(integration?this.text('集成候选')+' · ':'')+(artifact.taskId||artifact.id)));
      const code=this.node('code',artifact.contentHash||artifact.hash||'');code.style.overflowWrap='anywhere';row.append(code);
      row.append(this.renderArtifactDiff(artifact));
      if(artifact.isolation==='local_snapshot')row.append(this.node('p',this.text('本地快照检查，不等同于操作系统沙箱。'),'muted'));
      const review=artifact.review;
      if(review){
        const label=review.independent===false?'非独立审查':review.independent===true?'独立审查':'审查';
        row.append(this.node('p',this.text(label)+' · '+this.status(review.verdict||'unknown')+' · '+this.text(TEAM_SWARM_STATUS[review.validity]||'有效性未知'),'muted'));
      }else if(this.contract.requireIndependentReview)row.append(this.node('p',this.text('未取得独立审查结论'),'muted'));
      if(!integration){
        const input=this.node('input');input.type='checkbox';input.checked=this.state.selectedArtifacts.has(artifact.id);input.setAttribute('aria-label',this.text('选择要集成的候选')+' '+artifact.id);
        input.addEventListener('change',()=>{if(input.checked)this.state.selectedArtifacts.add(artifact.id);else this.state.selectedArtifacts.delete(artifact.id);if(this.integrateButton){this.integrateButton._swarmBlocked=this.blocked||this.state.selectedArtifacts.size<2;this.sync();}});
        row.prepend(this.control(input,this.blocked));
      }
      box.append(row);
    }
    if(candidates.length>1){
      box.append(this.node('p',this.text('至少选择两个候选；集成后仍须整体验证。'),'muted'));
      const actions=this.node('div',undefined,'team-run-actions');
      this.integrateButton=this.button('集成所选候选',()=>this.act('swarm/integrate',{artifactIds:[...this.state.selectedArtifacts]},'正在集成…'),this.blocked||this.state.selectedArtifacts.size<2);actions.append(this.integrateButton);box.append(actions);
    }
    return box;
  }
  renderEvals(){
    const box=this.node('div',undefined,'team-swarm-evals');box.append(this.node('h4',this.text('评测记录')));
    const evals=this.swarm.evals||[];
    if(!evals.length)box.append(this.node('p',this.text('还没有评测证据。'),'muted'));
    for(const record of evals){
      const row=this.node('article',undefined,'team-actions-card');row.dataset.evalId=record.id;
      row.append(this.node('strong',record.evalSpecId||record.id),this.badge(record.status));
      const validity=record.validity||'unknown';row.append(this.node('span',this.text(validity==='unknown'?'有效性未知':TEAM_SWARM_STATUS[validity]||validity),'team-badge '+(validity==='fresh'?'b-completed':'b-waiting_input')));
      if(validity!=='fresh')row.append(this.node('p',this.text('证据过期或有效性未知，不能用于当前验收。'),'muted'));
      if(record.reason)row.append(this.node('p',record.reason));
      const details=this.details('eval-'+record.id,'证据详情');
      if(record.artifactId)details.append(this.node('p',record.artifactId,'muted'));
      if(record.exitCode!==undefined&&record.exitCode!==null)details.append(this.node('p',this.text('退出码')+': '+record.exitCode));
      if(record.outputHead||record.output)details.append(this.node('pre',record.outputHead||record.output));
      if(record.truncated)details.append(this.node('p',this.text('输出已截断'),'muted'));
      const artifact=(this.swarm.artifacts||[]).find(value=>value.id===record.artifactId);
      if((record.isolation||artifact?.isolation)==='local_snapshot')details.append(this.node('p',this.text('本地快照检查，不等同于操作系统沙箱。'),'muted'));
      row.append(details);
      const active=['requested','pending','running','started'].includes(record.status);
      const actions=this.node('div',undefined,'team-run-actions');actions.append(this.button('复验此检查',()=>this.act('evals/'+encodeURIComponent(record.id)+'/recheck',{},'正在检查…'),this.blocked||active||!record.id));row.append(actions);box.append(row);
    }
    return box;
  }
  renderFindings(){
    const box=this.node('div',undefined,'team-swarm-findings'),findings=this.swarm.findings||[];
    if(!findings.length)return box;
    box.append(this.node('h4',this.text('待处理问题')));
    for(const finding of findings){const row=this.node('p');row.textContent=[finding.criterionId,finding.severity,finding.reason||finding.message||finding.description,finding.status].filter(Boolean).join(' · ');box.append(row);}
    return box;
  }
  renderBudget(){
    const budget=this.swarm.budget||{},contract=this.contract;
    const repairs=budget.repairsUsed??budget.repairs??this.swarm.repairCount??0;
    const evals=budget.evalsUsed??budget.evals??(this.swarm.evals||[]).length;
    this.root.append(this.node('p',this.text('修复次数')+': '+repairs+'/'+(budget.maxRepairs??contract.maxRepairs??'—')+' · '+this.text('评测次数')+': '+evals+'/'+(budget.maxEvals??contract.maxEvals??'—'),'muted'));
  }
}

if(typeof window!=='undefined')window.TeamSwarmPanel=TeamSwarmPanel;
if(typeof module!=='undefined'&&module.exports)module.exports={TeamSwarmPanel,teamSwarmCommand,teamSwarmBuildContract,teamSwarmText};
