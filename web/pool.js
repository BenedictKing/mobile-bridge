'use strict';
/* ACP 账号池只读面板（阶段 1）：展示 kimi 池内账号与窗口额度，
   支持从 zcode-switcher 导入与手动刷新额度。切换能力在后续阶段提供。 */
class PoolPanel {
  constructor({root,read,request}){
    Object.assign(this,{root,read,request});
    this.value=null;this.loading=false;this.busy=false;this.error='';
  }
  node(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
  button(text,fn){const n=this.node('button',text);n.type='button';n.onclick=fn;return n;}
  time(ms){return typeof ms==='number'&&ms>0?new Date(ms).toLocaleString():BridgeI18n.t('暂未提供');}
  resetTime(value){if(!value)return BridgeI18n.t('暂未提供');const t=new Date(value);return isNaN(t)?value:t.toLocaleString();}
  windowLabel(w){
    const t=BridgeI18n.t;
    if(w.windowUnit==='minute'&&w.windowDuration){
      const mins=w.windowDuration;
      if(mins%1440===0)return mins/1440+t(' 天');
      if(mins%60===0)return mins/60+t(' 小时');
      return mins+t(' 分钟');
    }
    return ({minute:t('分钟'),hour:t('小时'),day:t('天'),week:t('周')})[w.windowUnit]||t('额度窗口');
  }
  async refresh(){
    if(this.loading||this.busy)return;
    this.loading=true;this.error='';this.render();
    try{this.value=await this.read();}
    catch(error){this.error=error.message;}
    finally{this.loading=false;this.render();}
  }
  async perform(action){
    if(this.busy)return;
    this.busy=true;this.error='';this.render();
    try{this.value=await this.request(action);}
    catch(error){this.error=error.message;}
    finally{this.busy=false;this.render();}
  }
  quotaRows(card,row){
    const t=BridgeI18n.t,box=this.node('div',undefined,'accounts-usage');
    if(row.quotaError)box.append(this.node('span',row.quotaError,'error'));
    const quota=row.quota;
    if(!quota){box.append(this.node('span',row.quotaError?t('额度读取失败'):t('尚未读取额度'),'account-muted'));}
    else{
      for(const w of quota.limits||[]){
        const line=this.node('div',undefined,'accounts-window');
        const pct=w.limit>0?Math.max(0,100-Math.round(w.used/w.limit*1000)/10):null;
        line.append(this.node('span',(w.name||'')+this.windowLabel(w)),
          this.node('strong',pct==null?t('暂未提供'):t('剩余 ')+pct+'%'));
        if(pct!=null){const progress=this.node('progress');progress.max=100;progress.value=pct;progress.setAttribute('aria-label',t('剩余额度'));line.append(progress);}
        if(w.resetAt)line.title=t('恢复时间：')+this.resetTime(w.resetAt);
        box.append(line);
      }
      if(quota.weekly&&quota.weekly.limit>0){
        const w=quota.weekly,line=this.node('div',undefined,'accounts-window');
        const pct=Math.max(0,100-Math.round(w.used/w.limit*1000)/10);
        line.append(this.node('span',t('周额度')),this.node('strong',t('剩余 ')+pct+'%'));
        const progress=this.node('progress');progress.max=100;progress.value=pct;progress.setAttribute('aria-label',t('剩余额度'));line.append(progress);
        if(w.resetAt)line.title=t('恢复时间：')+this.resetTime(w.resetAt);
        box.append(line);
      }
      if(!quota.limits?.length&&!quota.weekly)box.append(this.node('span',t('暂未提供额度信息'),'account-muted'));
      box.append(this.node('p',t('更新于：')+this.time(row.quotaUpdatedAt),'account-muted'));
    }
    card.append(box);
  }
  render(){
    const t=BridgeI18n.t,root=this.root;root.replaceChildren();
    root.append(this.node('h3',t('ACP 账号池（Kimi）')));
    const controls=this.node('div',undefined,'pool-controls');
    const refresh=this.button(t('刷新额度'),()=>this.perform('refresh'));refresh.disabled=this.busy||this.loading;
    const importBtn=this.button(t('从 zcode-switcher 导入'),()=>{
      if(window.confirm(t('从 zcode-switcher 的账号库导入 Kimi 账号快照，并捕获当前登录态？')))this.perform('import');
    });importBtn.disabled=this.busy||this.loading;
    controls.append(refresh,importBtn);root.append(controls);
    if(this.error)root.append(this.node('p',this.error,'error'));
    if(this.loading&&!this.value){root.append(this.node('p',t('正在读取账号池…'),'account-muted'));return;}
    const value=this.value;
    if(!value)return;
    if(value.imported||value.skipped?.length){
      const note=this.node('p',t('导入完成：新增 ')+(value.imported||[]).length+t(' 个，跳过 ')+(value.skipped||[]).length+t(' 个'),'account-muted');
      root.append(note);
      for(const row of value.skipped||[])root.append(this.node('p',row.id+' · '+row.reason,'account-muted'));
    }
    if(value.captureError)root.append(this.node('p',t('当前登录态捕获失败：')+value.captureError,'account-muted'));
    if(value.refreshErrors)for(const[id,msg]of Object.entries(value.refreshErrors))root.append(this.node('p',id+' · '+msg,'error'));
    const accounts=value.accounts||[];
    if(!accounts.length){root.append(this.node('p',t('账号池为空。点击上方按钮从 zcode-switcher 导入，或捕获当前 Kimi 登录态。'),'account-muted'));return;}
    for(const row of [...accounts].sort((a,b)=>Number(b.active)-Number(a.active))){
      const card=this.node('section',undefined,'account-bucket');
      const identity=this.node('div',undefined,'accounts-identity'),name=this.node('div',undefined,'accounts-name');
      const heading=this.node('h3');heading.textContent=row.label||row.id;
      const title=this.node('div',undefined,'accounts-name-heading');title.append(heading);
      if(row.active)title.append(this.node('span',t('当前使用'),'accounts-credit'));
      if(row.status&&row.status!=='active')title.append(this.node('span',({reauth_required:t('需重新登录'),disabled:t('已停用'),cooling_down:t('冷却中')})[row.status]||row.status,'error'));
      name.append(title,this.node('p',row.email||row.nickname||row.userId||row.id,'account-muted'));
      identity.append(name);this.quotaRows(identity,row);card.append(identity);
      root.append(card);
    }
  }
}
if(typeof module!=='undefined')module.exports={PoolPanel};
