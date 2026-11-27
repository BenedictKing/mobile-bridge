'use strict';

class WorkModes {
  constructor({select,control,goalRoot,goalToggle,storage=sessionStorage,onChange,onCancel,onStatus,onEdit,onContinue,onComplete,onReview,newId=()=>crypto.randomUUID()}) {
    this.select=select;this.control=control;this.items=[...control.querySelectorAll('[data-work-mode]')];this.goalRoot=goalRoot;this.goalToggle=goalToggle;this.storage=storage;this.onChange=onChange;this.onCancel=onCancel;this.onStatus=onStatus;this.onEdit=onEdit;this.onContinue=onContinue;this.onComplete=onComplete;this.onReview=onReview;this.newId=newId;this.sending=false;this.editing=false;this.editText="";
    this.key=null;this.selection=null;this.view=null;this.cancelBusy=false;this.statusBusy=null;this.actionError='';
    select.onchange=()=>{this.selection=select.value;this.storage.setItem('work-mode:'+this.key,this.selection);this.actionError='';this.render();this.onChange?.(this.selection);};
    goalToggle.onclick=()=>{this.storage.removeItem('hidden-goal:'+this.key);this.render();this.goalRoot.querySelector('.goal-close')?.focus();};
    for(const item of this.items)item.onclick=()=>{if(item.disabled||this.select.disabled)return;this.select.value=item.dataset.workMode;this.select.onchange();};
  }
  open(key) {
    this.key=key;const saved=this.storage.getItem('work-mode:'+key);
    this.selection=['default','plan','goal'].includes(saved)?saved:null;
    this.view=null;this.cancelBusy=false;this.statusBusy=null;this.sending=false;this.editing=false;this.editText='';this.actionError='';this.render();
  }
  value(sendMode) {return sendMode==='steer'?null:this.select.value;}
  submitted(key,mode) {
    if(mode!=='goal')return;
    this.storage.setItem('work-mode:'+key,'default');
    if(key===this.key){this.selection='default';this.cancelBusy=false;this.statusBusy=null;this.actionError='';this.render();}
  }
  async action(kind,fields,run) {
    if(this.busy())return;
    const key=this.key,storageKey='goal-action:'+key;
    const goal=this.view?.goal;
    const expected=goal?Object.fromEntries(['objective','status','createdAt','createdAtMs','goalId'].filter(k=>goal[k]!=null).map(k=>[k,goal[k]])):{};
    const fingerprint=JSON.stringify({kind,fields,expected});
    let pending;try{pending=JSON.parse(this.storage.getItem(storageKey));}catch{}
    if(!pending||pending.fingerprint!==fingerprint)pending={fingerprint,payload:{id:this.newId(),...fields,expected,uiLocale:BridgeI18n.locale(),...(goal?.provider==='gateway'?{revision:goal.revision,expectedState:goal.status}:{})}};
    this.storage.setItem(storageKey,JSON.stringify(pending));
    this.cancelBusy=kind==='cancel';this.statusBusy=kind==='cancel'?null:kind;this.actionError='';this.render();
    try{await run(pending.payload);this.storage.removeItem(storageKey);if(this.key===key)this.editing=false;}
    catch(error){if(this.key===key)this.actionError=BridgeI18n.t(error.message||'目标操作失败，请重试');}
    finally{if(this.key===key){this.cancelBusy=false;this.statusBusy=null;this.render();}}
  }
  cancel(){return this.action('cancel',{},payload=>this.onCancel(payload));}
  setStatus(status){return this.action(status,{status},payload=>this.onStatus(payload));}
  saveEdit(){return this.action('edit',{objective:this.editText.trim()},payload=>this.onEdit(payload));}
  continueOnce(){return this.action('continue',{},payload=>this.onContinue(payload));}
  complete(){return this.action('complete',{},payload=>this.onComplete(payload));}
  review(){return this.action('review',{verified:true},payload=>this.onReview(payload));}
  busy(){return !!(this.sending||this.cancelBusy||this.statusBusy);}
  render(view=this.view,sendMode=this.sendMode,sending=this.sending) {
    this.view=view;this.sendMode=sendMode;this.sending=!!sending;
    const busy=this.busy();
    const t=BridgeI18n.t,hasGoals=(view?.capabilities||[]).includes('goals');
    const goalSupported=hasGoals&&!!view&&view.goalRuntimeAvailable!==false;
    const goalUnavailableReason=!hasGoals?'当前 Agent 不支持目标模式':'目标模式运行时暂不可用';
    this.select.value=this.selection||(view?.collaborationMode==='plan'?'plan':'default');
    if(!goalSupported&&this.select.value==='goal')this.select.value='default';
    this.select.disabled=!view||view.loadingHistory||view.activating||busy||sendMode==='steer';
    const goalOption=this.select.querySelector('[value=goal]');
    goalOption.hidden=!hasGoals;
    goalOption.disabled=!goalSupported||!!(view?.status==='active'||(view?.goal&&!['complete','completed','cancelled'].includes(view.goal.status))||['pending','unknown'].includes(view?.goalSubmission?.status));
    goalOption.title=t(goalSupported?(view?.status==='active'?'当前回复结束后可开启目标模式':''):goalUnavailableReason);
    if(this.control){
      for(const item of this.items){
        const mode=item.dataset.workMode,option=this.select.querySelector('[value="'+mode+'"]');
        const disabled=this.select.disabled||!!option?.disabled;
        item.hidden=!!option?.hidden;item.disabled=disabled;
        item.classList.toggle('active',mode===this.select.value);
        item.setAttribute('aria-pressed',String(mode===this.select.value));
        item.title=mode==='goal'&&option?.disabled?option.title:'';
      }
      this.control.setAttribute('aria-disabled',String(this.select.disabled));
    }
    const goal=view?.goal,request=view?.goalSubmission;
    const goalIdentity=request?JSON.stringify(['request',request.id]):goal?JSON.stringify(['goal',goal.goalId||goal.createdAt,goal.objective]):null;
    const dismissed=!!goalIdentity&&this.storage.getItem('hidden-goal:'+this.key)===goalIdentity;
    this.goalToggle.hidden=!(goal||request);
    const expanded=this.goalRoot.firstElementChild?.open||false;
    this.goalRoot.replaceChildren();this.goalRoot.hidden=!goalIdentity||dismissed;
    const node=(tag,text,cls)=>{const el=document.createElement(tag);el.textContent=text;if(cls)el.className=cls;return el;};
    const button=(text,cls,onclick,disabled)=>{const el=node('button',text,cls);el.type='button';el.disabled=!!disabled;el.onclick=onclick;return el;};
    let goalDetails=null;
    if(goal){
      const labels={active:'目标进行中',dispatching:'目标正在派发',evaluating:'目标正在评估',waiting_input:'目标等待回答',waiting_review:'目标等待复核',outcome_unknown:'目标派发结果未知',completed:'目标已完成',cancelled:'目标已取消',paused:'目标已暂停',blocked:'目标等待处理',usageLimited:'目标已达到用量限制',budgetLimited:'目标已达到预算限制',complete:'目标已完成'};
      goalDetails=document.createElement('details');goalDetails.open=expanded;
      goalDetails.append(node('summary',t(labels[goal.status]||'目标状态')));
      goalDetails.append(node('p',goal.objective,'goal-objective'));
      if(goal.provider==='gateway'){
        goalDetails.append(node('p',t('剩余自动轮数：')+Math.max(0,goal.autoStepsLimit-goal.autoStepsUsed),'muted'));
        if(goal.stopReason)goalDetails.append(node('p',goal.stopReason,'muted'));
        if(goal.status==='outcome_unknown')goalDetails.append(node('p',t('请核实上一轮是否已送达；网关不会自动重发。'),'muted'));
        goalDetails.append(node('p',t('目标和最近输出摘要会发送给已配置的 Jev 服务。'),'muted'));
      }
      if(Number.isFinite(goal.tokensUsed))goalDetails.append(node('p',t('已用 Token：')+goal.tokensUsed.toLocaleString()+(Number.isFinite(goal.tokenBudget)?' / '+goal.tokenBudget.toLocaleString():''),'muted'));
      this.goalRoot.append(goalDetails);
    }
    if(request){
      const labels={pending:'正在开启原生目标…',unknown:'目标请求送达结果尚未确认，请查看会话。',unconfirmed:'尚未确认目标已开启，请刷新或查看 Codex。'};
      this.goalRoot.append(node('p',t(labels[request.status]||labels.unknown),'goal-pending'));
    }
    if(this.actionError)(goalDetails||this.goalRoot).append(node('p',this.actionError,'error'));
    if(goal&&goalSupported){
      const actions=node('div','','goal-actions');
      goalDetails.append(actions);
      if(['active','blocked','waiting_input','waiting_review','evaluating','dispatching','usageLimited','budgetLimited'].includes(goal.status)){
        actions.append(button(this.statusBusy==='paused'?t('正在暂停目标…'):t('暂停目标'),'goal-status secondary',()=>this.setStatus('paused'),this.busy()));
      }
      if(goal.status==='paused'){
        actions.append(button(this.statusBusy==='active'?t('正在恢复目标…'):t('恢复目标'),'goal-status primary',()=>this.setStatus('active'),this.busy()));
      }
      if(goal.provider==='gateway'&&['waiting_review','blocked','paused'].includes(goal.status)){
        actions.append(button(t('人工继续一轮'),'goal-continue primary',()=>this.continueOnce(),this.busy()));
      }
      if(goal.provider==='gateway'&&goal.status==='waiting_review')actions.append(button(t('标记完成'),'goal-complete secondary',()=>this.complete(),this.busy()));
      if(goal.provider==='gateway'&&goal.status==='outcome_unknown')actions.append(button(t('已核实，进入复核'),'goal-review secondary',()=>this.review(),this.busy()));
      if(goal.status==='paused')actions.append(button(t('修改目标'),'goal-edit secondary',()=>{this.editing=true;this.editText=goal.objective;this.render();this.goalRoot.querySelector('.goal-editor')?.focus();},this.busy()));
      if(!['complete','completed','cancelled'].includes(goal.status))actions.append(button(this.cancelBusy?t('正在关闭目标…'):t('关闭目标'),'goal-cancel secondary',()=>this.cancel(),this.busy()));
      if(this.editing&&goal.status==='paused'){
        const input=node('textarea','', 'goal-editor');input.value=this.editText;input.maxLength=4000;input.rows=3;input.disabled=this.busy();input.setAttribute('aria-label',t('目标内容'));input.oninput=()=>{this.editText=input.value;};
        goalDetails.append(input,node('p',t('修改目标内容会重置用量统计，保存后保持暂停。'),'muted'));
        goalDetails.append(button(t('保存目标'),'goal-save primary',()=>this.saveEdit(),this.busy()),button(t('放弃修改'),'secondary',()=>{this.editing=false;this.render();},this.busy()));
      }
    }
    const terminalGoal=goal&&['complete','completed','cancelled'].includes(goal.status);
    const dismissibleGoal=terminalGoal||(!goal&&request&&['unknown','unconfirmed'].includes(request.status));
    if(goalIdentity&&dismissibleGoal){
      const close=node('button',null,'goal-close');close.append(BridgeIcons.close());close.type='button';close.title=t('隐藏目标栏');close.setAttribute('aria-label',close.title);
      close.onclick=()=>{this.storage.setItem('hidden-goal:'+this.key,goalIdentity);this.render();this.goalToggle.focus();};
      this.goalRoot.append(close);
    }
  }
}

if(typeof module!=='undefined')module.exports={WorkModes};
