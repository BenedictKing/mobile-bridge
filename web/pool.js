'use strict';
/* ACP 账号池面板：展示池内账号与窗口额度，支持捕获本机当前登录态
   （新账号导入 / 已有账号更新凭证）、删除快照、网关内登录
   （device flow / 扫码 / OAuth 轮询）、手动刷新额度、切换账号与自动切换开关。
   kimi/zcode/minimax/workbuddy 各实例化一个。 */

/* 各 kind 的登录配置：providers 非空时登录前先选择（minimax/workbuddy 的
   区域/档位、zcode 的 OAuth provider）。 */
const POOL_LOGIN={
  kimi:{},
  minimax:{providers:[{value:'cn',label:'国内版'},{value:'en',label:'国际版'}]},
  workbuddy:{providers:[{value:'cn',label:'国内版（CodeBuddy）'},{value:'ai',label:'国际版（WorkBuddy AI）'}]},
  zcode:{providers:[{value:'zai',label:'z.ai'},{value:'bigmodel',label:'BigModel（智谱）'}]},
};

class PoolPanel {
  constructor({root,read,request,readLogin,kind,label}){
    Object.assign(this,{root,read,request,readLogin,kind,label});
    this.value=null;this.loading=false;this.busy=false;this.error='';this.notice='';
    this.login=null; // {session, challenge, provider, reauthId, timer, deadline}
  }
  node(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
  button(text,fn){const n=this.node('button',text);n.type='button';n.onclick=fn;return n;}
  time(ms){return typeof ms==='number'&&ms>0?new Date(ms).toLocaleString():BridgeI18n.t('暂未提供');}
  resetTime(value){if(!value)return BridgeI18n.t('暂未提供');const t=new Date(value);return isNaN(t)?value:t.toLocaleString();}
  // 相对时长：「X 天 X 小时后重置」；不足一分钟或已过期则提示刷新（悬浮仍保留绝对时间）。
  untilText(value,suffix){
    const t=BridgeI18n.t;
    if(!value)return '';
    const at=new Date(value).getTime();
    if(isNaN(at))return String(value);
    const minutes=Math.floor((at-Date.now())/60000);
    if(minutes<1)return t('等待刷新');
    const days=Math.floor(minutes/1440),hours=Math.floor(minutes%1440/60),rest=minutes%60;
    const parts=days>0?[days+t(' 天'),hours>0?hours+t(' 小时'):'']:
      hours>0?[hours+t(' 小时'),rest>0?rest+t(' 分钟'):'']:[rest+t(' 分钟')];
    return parts.filter(Boolean).join(' ')+t(suffix||'后重置');
  }
  windowLabel(w){
    const t=BridgeI18n.t;
    if(w.windowUnit==='minute'&&w.windowDuration){
      const mins=w.windowDuration;
      if(mins%1440===0)return mins/1440+t(' 天');
      if(mins%60===0)return mins/60+t(' 小时');
      return mins+t(' 分钟');
    }
    if(!w.windowUnit)return '';
    return ({minute:t('分钟'),hour:t('小时'),day:t('天'),week:t('周')})[w.windowUnit]||'';
  }
  async refresh(){
    if(this.loading||this.busy)return;
    this.loading=true;this.error='';this.render();
    try{this.value=await this.read();}
    catch(error){this.error=error.message;}
    finally{this.loading=false;this.render();}
  }
  async perform(action,payload={}){
    if(this.busy)return;
    this.busy=true;this.error='';this.render();
    try{this.value=await this.request(action,payload);}
    catch(error){this.error=error.message;}
    finally{this.busy=false;this.render();}
  }
  async switchTo(row){
    const t=BridgeI18n.t;
    if(!window.confirm(t('确认切换账号？进行中的回合会被拒绝，已打开的会话将以新账号重连。')+'\n'+(row.label||row.id)))return;
    await this.perform('switch',{id:row.id,confirmed:true});
  }
  removeAccount(row){
    const t=BridgeI18n.t;
    if(!window.confirm(t('仅删除池中保存的账号快照，不影响本机当前的登录态。确认删除？')+'\n'+(row.label||row.id)))return;
    this.perform('remove',{id:row.id,confirmed:true});
  }
  async capture(){
    const t=BridgeI18n.t;
    await this.perform('capture');
    const captured=this.value&&this.value.captured;
    if(captured&&captured.id){
      this.notice=(captured.created?t('已导入当前登录账号：'):t('已从当前登录态更新账号：'))+captured.id;
      if(captured.envelope)this.notice+=t('（凭据为加密信封态，请点「添加账号」扫码重新登录获取明文凭据后才能查询额度与切换）');
      this.render();
    }
  }
  stopLoginPolling(){
    if(this.login&&this.login.timer){clearInterval(this.login.timer);}
    if(this.login)this.login.timer=null;
  }
  openLogin(reauthId){
    const t=BridgeI18n.t;
    const config=POOL_LOGIN[this.kind];
    if(!config)return;
    this.stopLoginPolling();
    this.login={session:null,challenge:null,reauthId:reauthId||'',provider:config.providers?config.providers[0].value:'',phase:'pick',error:''};
    if(!config.providers)this.startLogin();
    this.render();
  }
  async startLogin(){
    const t=BridgeI18n.t,login=this.login;
    if(!login)return;
    login.phase='waiting';login.error='';this.render();
    try{
      const result=await this.request('login',{provider:login.provider,reauthId:login.reauthId});
      login.session=result.login;login.challenge=result.challenge||{};
      login.deadline=Date.now()+(login.challenge.expiresIn||600)*1000;
      this.render();
      this.stopLoginPolling();
      login.timer=setInterval(()=>this.pollLogin(),2000);
    }catch(error){login.phase='pick';login.error=error.message;this.render();}
  }
  async pollLogin(){
    const t=BridgeI18n.t,login=this.login;
    if(!login||!this.readLogin)return;
    try{
      const result=await this.readLogin();
      const session=result.login;
      if(!session)return;
      login.session=session;
      if(session.status==='done'){
        this.stopLoginPolling();
        const accountId=session.accountId||'';
        this.login=null;
        await this.refresh();
        this.notice=t('登录成功')+(accountId?'：'+accountId:'');
        this.render();
        return;
      }
      if(session.status==='failed'||session.status==='cancelled'){
        this.stopLoginPolling();
        login.phase='pick';
        login.error=session.error||t('登录未完成');
        login.session=null;this.render();
        return;
      }
      if(login.deadline&&Date.now()>login.deadline)this.render();
    }catch(error){/* 会话已被取代或网关重启：保持等待，下轮再试 */}
  }
  async cancelLogin(){
    const login=this.login;
    this.stopLoginPolling();
    try{await this.request('login/cancel');}catch(error){/* 忽略 */}
    this.login=null;this.render();
  }
  loginBox(){
    const t=BridgeI18n.t,login=this.login;
    const box=this.node('section',undefined,'account-bucket pool-login');
    const config=POOL_LOGIN[this.kind];
    if(login.phase==='pick'){
      box.append(this.node('h3',login.reauthId?t('重新登录'):t('添加账号')));
      if(config.providers){
        const label=this.node('label',undefined,'pool-login-provider');
        const select=document.createElement('select');
        for(const p of config.providers){const option=document.createElement('option');option.value=p.value;option.textContent=t(p.label);select.append(option);}
        select.value=login.provider;select.onchange=()=>{login.provider=select.value;};
        label.append(select);box.append(label);
      }
      if(login.error)box.append(this.node('p',login.error,'error'));
      const start=this.button(t('开始登录'),()=>this.startLogin());
      const cancel=this.button(t('取消'),()=>{this.login=null;this.render();});
      box.append(start,cancel);
      return box;
    }
    const challenge=login.challenge||{};
    box.append(this.node('h3',t('等待完成登录…')));
    if(challenge.userCode){
      const code=this.node('p',undefined,'pool-login-code');
      code.append(this.node('strong',challenge.userCode));
      box.append(code);
    }
    if(challenge.url){
      const link=this.node('a',challenge.url);link.href=challenge.url;link.target='_blank';link.rel='noopener noreferrer';
      const line=this.node('p');line.append(link);box.append(line);
      const copy=this.button(t('复制链接'),()=>{navigator.clipboard?.writeText(challenge.url);});
      box.append(copy);
    }
    if(login.deadline){
      const left=Math.max(0,Math.round((login.deadline-Date.now())/1000));
      box.append(this.node('p',t('请在有效期内完成授权，剩余约 ')+Math.floor(left/60)+t(' 分钟'),'account-muted'));
    }
    const cancel=this.button(t('取消登录'),()=>this.cancelLogin());
    box.append(cancel);
    return box;
  }
  quotaRows(card,row){
    const t=BridgeI18n.t,box=this.node('div',undefined,'accounts-usage');
    if(row.quotaError)box.append(this.node('span',row.quotaError,'error'));
    const quota=row.quota;
    if(!quota){box.append(this.node('span',row.quotaError?t('额度读取失败'):t('尚未读取额度'),'account-muted'));}
    else{
      const membership=quota.membership;
      if(membership&&membership.hasTokenPlan===false)box.append(this.node('span',t('未订阅 Coding Plan'),'error'));
      else if(membership&&membership.tier)box.append(this.node('span',membership.tier,'account-muted'));
      const resets=quota.resetCredits;
      if(this.kind==='zcode'&&resets){
        const line=this.node('div',undefined,'accounts-window');
        line.append(this.node('span',t('重置卡')),this.node('strong',resets.availableCount));
        line.append(this.node('span',t('5 小时：')+resets.fiveHourCards+' · '+t('周卡：')+resets.weekCards,'account-muted'));
        box.append(line);
      }
      const credits=quota.credits;
      if(credits){
        const line=this.node('div',undefined,'accounts-window');
        const pct=credits.totalCapacity>0?Math.min(100,Math.max(0,Math.round((credits.totalCapacity-credits.totalRemaining)/credits.totalCapacity*1000)/10)):null;
        line.append(this.node('span',t('积分')+(credits.expired?' · '+t('已到期'):'')+(credits.expiringSoon?' · '+t('7 天内到期'):'')),
          this.node('strong',pct==null?t('暂未提供'):t('已用 ')+pct+'%'));
        if(pct!=null){const progress=this.node('progress');progress.max=100;progress.value=pct;progress.setAttribute('aria-label',t('已用额度'));line.append(progress);}
        if(credits.soonestExpireAt){const left=this.untilText(credits.soonestExpireAt,'后到期');line.title=t('到期时间：')+this.time(credits.soonestExpireAt);if(left)line.append(this.node('span',left,'account-muted'));}
        box.append(line);
        for(const r of credits.resources||[]){
          if(!r.packageName&&!r.packageCode)continue;
          const item=this.node('div',undefined,'accounts-window');
          const usedPct=r.total>0?Math.min(100,Math.max(0,Math.round((r.total-r.remaining)/r.total*1000)/10)):null;
          item.append(this.node('span',(r.packageName||r.packageCode)+(r.expiringSoon?' · '+t('7 天内到期'):'')+(r.expired?' · '+t('已到期'):'')),
            this.node('strong',usedPct==null?t('暂未提供'):t('已用 ')+usedPct+'%'));
          if(usedPct!=null){const progress=this.node('progress');progress.max=100;progress.value=usedPct;progress.setAttribute('aria-label',t('已用额度'));item.append(progress);}
          if(r.expireAt){const left=this.untilText(r.expireAt,'后到期');item.title=t('到期时间：')+this.time(r.expireAt);if(left)item.append(this.node('span',left,'account-muted'));}
          box.append(item);
        }
      }
      for(const w of quota.limits||[]){
        const line=this.node('div',undefined,'accounts-window');
        const pct=w.limit>0?Math.min(100,Math.max(0,Math.round(w.used/w.limit*1000)/10)):null;
        line.append(this.node('span',t(w.name||'')+this.windowLabel(w)+(w.auxiliary?' · '+t('辅助'):'')),
          this.node('strong',pct==null?t('暂未提供'):t('已用 ')+pct+'%'));
        if(w.auxiliary)line.firstChild.title=t('辅助，不影响切换');
        if(pct!=null){const progress=this.node('progress');progress.max=100;progress.value=pct;progress.setAttribute('aria-label',t('已用额度'));line.append(progress);}
        if(w.resetAt){const left=this.untilText(w.resetAt,'后重置');line.title=t('恢复时间：')+this.resetTime(w.resetAt);if(left)line.append(this.node('span',left,'account-muted'));}
        box.append(line);
      }
      if(quota.weekly&&quota.weekly.limit>0){
        const w=quota.weekly,line=this.node('div',undefined,'accounts-window');
        const pct=Math.min(100,Math.max(0,Math.round(w.used/w.limit*1000)/10));
        line.append(this.node('span',t('周额度')),this.node('strong',t('已用 ')+pct+'%'));
        const progress=this.node('progress');progress.max=100;progress.value=pct;progress.setAttribute('aria-label',t('已用额度'));line.append(progress);
        if(w.resetAt){const left=this.untilText(w.resetAt,'后重置');line.title=t('恢复时间：')+this.resetTime(w.resetAt);if(left)line.append(this.node('span',left,'account-muted'));}
        box.append(line);
      }
      if(!quota.limits?.length&&!quota.weekly)box.append(this.node('span',t('暂未提供额度信息'),'account-muted'));
      box.append(this.node('p',t('更新于：')+this.time(row.quotaUpdatedAt),'account-muted'));
    }
    card.append(box);
  }
  render(){
    const t=BridgeI18n.t,root=this.root;root.replaceChildren();
    // 标签已写明是哪个池，这里不再重复“ACP 账号池（xx）”标题。
    if(this.error)root.append(this.node('p',this.error,'error'));
    if(this.loading&&!this.value){root.append(this.node('p',t('正在读取账号池…'),'account-muted'));return;}
    const value=this.value;
    if(!value)return;
    if(value.readOnly){
      // Codex 等只读视图：展示当前账号，写操作指向既有管理面板。
      const accounts=value.accounts||[];
      root.append(this.node('p',t('只读视图。Codex 账号的切换与本面板上方的「账号与接入」管理流程一致。'),'account-muted'));
      for(const row of accounts){
        const card=this.node('section',undefined,'account-bucket');
        const identity=this.node('div',undefined,'accounts-identity'),name=this.node('div',undefined,'accounts-name');
        const heading=this.node('h3');heading.textContent=row.name||row.label||row.id;
        const title=this.node('div',undefined,'accounts-name-heading');title.append(heading);
        if(row.id===value.activeId)title.append(this.node('span',t('当前使用'),'accounts-credit'));
        name.append(title,this.node('p',row.email||[row.baseUrl,row.model].filter(Boolean).join(' · ')||row.id,'account-muted'));
        identity.append(name);card.append(identity);
        root.append(card);
      }
      if(!accounts.length)root.append(this.node('p',t('暂无已保存的账号。'),'account-muted'));
      return;
    }
    const controls=this.node('div',undefined,'pool-controls');
    const refresh=this.button(t('刷新额度'),()=>this.perform('refresh'));refresh.disabled=this.busy||this.loading;
    const captureBtn=this.button(t('捕获当前登录'),()=>this.capture());
    captureBtn.disabled=this.busy||this.loading;
    captureBtn.title=t('读取本机 CLI 当前的登录态：新账号直接导入，已有账号则更新其登录凭证');
    controls.append(refresh,captureBtn);
    if(POOL_LOGIN[this.kind]){
      const addBtn=this.button(t('添加账号'),()=>this.openLogin(''));
      addBtn.disabled=this.busy||this.loading;
      controls.append(addBtn);
    }
    const autoLabel=this.node('label',undefined,'notification-option');
    this.autoSwitch=document.createElement('input');this.autoSwitch.type='checkbox';
    this.autoSwitch.onchange=()=>{this.autoSwitch.disabled=true;this.perform('settings',{autoSwitch:this.autoSwitch.checked});};
    autoLabel.append(this.autoSwitch,this.node('strong',t('额度用尽时自动切换并继续')));
    controls.append(autoLabel);root.append(controls);
    this.autoSwitch.checked=value.autoSwitch!==false;
    this.autoSwitch.disabled=this.busy||this.loading;
    if(this.notice){root.append(this.node('p',this.notice,'accounts-credit'));this.notice='';}
    if(this.login)root.append(this.loginBox());
    if(value.captureError)root.append(this.node('p',t('当前登录态捕获失败：')+value.captureError,'account-muted'));
    if(value.refreshErrors)for(const[id,msg]of Object.entries(value.refreshErrors))root.append(this.node('p',id+' · '+msg,'error'));
    const accounts=value.accounts||[];
    if(!accounts.length){root.append(this.node('p',t('账号池为空。点击上方「捕获当前登录」导入本机已登录的账号。'),'account-muted'));return;}
    for(const row of [...accounts].sort((a,b)=>Number(b.active)-Number(a.active))){
      const card=this.node('section',undefined,'account-bucket');
      const identity=this.node('div',undefined,'accounts-identity'),name=this.node('div',undefined,'accounts-name');
      const heading=this.node('h3');heading.textContent=row.label||row.id;
      const title=this.node('div',undefined,'accounts-name-heading');title.append(heading);
      if(row.active)title.append(this.node('span',t('当前使用'),'accounts-credit'));
      if(row.status&&row.status!=='active')title.append(this.node('span',({reauth_required:t('需重新登录'),disabled:t('已停用'),cooling_down:t('冷却中')})[row.status]||row.status,'error'));
      if(row.region)title.append(this.node('span',String(row.region).toUpperCase(),'account-muted'));
      if(row.tokenEnvelope)title.append(this.node('span',t('信封态'),'error'));
      // 副标题与账号名相同时不再重复显示。
      const subtitle=row.email||row.nickname||row.userId||row.provider||row.id;
      name.append(title);
      if(subtitle&&subtitle!==heading.textContent&&String(subtitle).toUpperCase()!==String(row.region||'').toUpperCase())name.append(this.node('p',subtitle,'account-muted'));
      identity.append(name);this.quotaRows(identity,row);card.append(identity);
      // 当前账号已有“当前使用”标记，不再放一个禁用按钮。
      if(!row.active){
        const choose=this.button(t('切换到此账号'),()=>this.switchTo(row));
        choose.disabled=this.busy||this.loading||(row.status&&row.status!=='active');
        card.append(choose);
      }
      const remove=this.button(t('删除'),()=>this.removeAccount(row));
      remove.disabled=this.busy||this.loading;
      card.append(remove);
      if(row.status==='reauth_required'&&POOL_LOGIN[this.kind]){
        const reauth=this.button(t('重新登录'),()=>this.openLogin(row.id));
        reauth.disabled=this.busy||this.loading;
        card.append(reauth);
      }
      root.append(card);
    }
    const history=value.history||[];
    if(history.length){
      const box=this.node('details',undefined,'pool-history');
      box.append(this.node('summary',t('切换历史')));
      for(const record of history){
        const line=this.node('p',undefined,'account-muted');
        line.textContent=this.time(record.at)+' · '+(record.from||t('（未登录）'))+' → '+record.to+
          ' · '+((record.trigger==='manual'?t('手动'):record.trigger)||'')+(record.ok?'':' · '+t('失败：')+(record.error||''));
        box.append(line);
      }
      root.append(box);
    }
  }
}
if(typeof module!=='undefined')module.exports={PoolPanel};
