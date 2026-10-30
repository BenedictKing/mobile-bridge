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

/* 国际版（WorkBuddy AI）新号要先提交地区再领一次性 trial 才能对话，
   否则腾讯回 14017 trial not activated。地区由使用者决定，这里只给官网短名单。 */
const WORKBUDDY_AI_REGIONS=[
  {code:'HK',label:'中国香港'},{code:'MO',label:'中国澳门'},{code:'SG',label:'新加坡'},
  {code:'TH',label:'泰国'},{code:'PH',label:'菲律宾'},{code:'MY',label:'马来西亚'},{code:'ID',label:'印度尼西亚'},
];

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
  spanText(minutes){
    const t=BridgeI18n.t;
    const days=Math.floor(minutes/1440),hours=Math.floor(minutes%1440/60),rest=minutes%60;
    const parts=days>0?[days+t(' 天'),hours>0?hours+t(' 小时'):'']:
      hours>0?[hours+t(' 小时'),rest>0?rest+t(' 分钟'):'']:[rest+t(' 分钟')];
    return parts.filter(Boolean).join(' ');
  }
  untilText(value,suffix){
    const t=BridgeI18n.t;
    if(!value)return '';
    const at=new Date(value).getTime();
    if(isNaN(at))return String(value);
    const minutes=Math.floor((at-Date.now())/60000);
    if(minutes<1)return t('等待刷新');
    return this.spanText(minutes)+t(suffix||'后重置');
  }
  // 「X 分钟前」；不足一分钟视为刚刚（悬浮保留绝对时间）。
  agoText(ms){
    const t=BridgeI18n.t;
    if(typeof ms!=='number'||!(ms>0))return '';
    const minutes=Math.floor((Date.now()-ms)/60000);
    if(minutes<1)return t('刚刚');
    return this.spanText(minutes)+t('前');
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
  // CodeBuddy CN 每日签到：成功后后端顺带刷新额度，回执 message 作为通知。
  async checkin(row){
    await this.perform('checkin',{id:row.id});
    const done=this.value&&this.value.checkin;
    if(done&&done.message){this.notice=done.message;this.render();}
  }
  // 国际版待开通账号：提交所选地区并领取一次性 trial。
  async provision(row,region){
    await this.perform('provision',{id:row.id,region});
    const done=this.value&&this.value.provision;
    if(done&&done.message){this.notice=done.message;this.render();}
  }
  async capture(){
    const t=BridgeI18n.t;
    await this.perform('capture');
    const captured=this.value&&this.value.captured;
    if(captured&&captured.id){
      this.notice=(captured.created?t('已导入当前登录账号：'):t('已从当前登录态更新账号：'))+captured.id;
      // 5.6 起桌面在磁盘上只留 $wbEncrypted 信封：捕获只能拿到密文，
      // 既读不到额度也切不了号，要明文必须走网关内扫码登录。
      if(captured.envelope){
        this.notice=t('捕获到的是 WorkBuddy 加密信封凭据，读不到额度；请在该账号上点「扫码重新登录」换一份明文凭据。');
        this.noticeError=true;
      }
      this.render();
    }
  }
  stopLoginPolling(){
    if(this.login&&this.login.timer){clearInterval(this.login.timer);}
    if(this.login)this.login.timer=null;
  }
  openLogin(reauthId,variant){
    const t=BridgeI18n.t;
    const config=POOL_LOGIN[this.kind];
    if(!config)return;
    this.stopLoginPolling();
    const providers=config.providers||[];
    // 重新登录时默认选中该账号原来的档位，别把 cn 的号登进 ai 的二维码。
    const preferred=providers.find(option=>option.value===String(variant||'').toLowerCase())||providers[0];
    this.login={session:null,challenge:null,reauthId:reauthId||'',provider:preferred?preferred.value:'',phase:'pick',error:''};
    if(!providers.length)this.startLogin();
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
    if(row.quotaError)box.append(this.node('span',t(row.quotaError),'error'));
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
        const asNumber=value=>typeof value==='number'?value:(Number(value)||0);
        const badges=(expired,expiringSoon)=>(expired?' · '+t('已到期'):'')+(expiringSoon?' · '+t('7 天内到期'):'');
        // 一条额度行：标题 + 「已用 x / y · z%」+ 进度条 + 到期倒计时。
        const addRow=(label,used,limit,flags,expireAt)=>{
          const line=this.node('div',undefined,'accounts-window');
          const pct=limit>0?Math.min(100,Math.max(0,Math.round(used/limit*1000)/10)):null;
          const amount=limit>0?Math.max(0,Math.round(used))+' / '+Math.round(limit):null;
          line.append(this.node('span',label+(flags||'')),
            this.node('strong',amount==null?t('暂未提供'):t('已用 ')+amount+(pct==null?'':' · '+pct+'%')));
          if(pct!=null){const progress=this.node('progress');progress.max=100;progress.value=pct;progress.setAttribute('aria-label',t('已用额度'));line.append(progress);}
          if(expireAt){const left=this.untilText(expireAt,'后到期');line.title=t('到期时间：')+this.time(expireAt);if(left)line.append(this.node('span',left,'account-muted'));}
          box.append(line);
        };
        const resources=(credits.resources||[]).filter(r=>r.packageName||r.packageCode);
        if(resources.length===1&&asNumber(resources[0].total)>0){
          // 只有一个积分包时，汇总行与明细行是同一组数字，只留带包名的一条。
          const r=resources[0],total=asNumber(r.total),remaining=asNumber(r.remaining);
          addRow(r.packageName||r.packageCode,total-remaining,total,badges(r.expired,r.expiringSoon),r.expireAt);
        }else{
          const total=asNumber(credits.totalCapacity),remaining=asNumber(credits.totalRemaining);
          addRow(t('积分'),total-remaining,total,badges(credits.expired,credits.expiringSoon),credits.soonestExpireAt);
          for(const r of resources){
            const itemTotal=asNumber(r.total),itemRemaining=asNumber(r.remaining);
            addRow(r.packageName||r.packageCode,itemTotal-itemRemaining,itemTotal,badges(r.expired,r.expiringSoon),r.expireAt);
          }
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
      if(!quota.limits?.length&&!quota.weekly&&!quota.credits)box.append(this.node('span',t('暂未提供额度信息'),'account-muted'));
      const updated=this.node('p',t('更新于：')+(this.agoText(row.quotaUpdatedAt)||this.time(row.quotaUpdatedAt)),'account-muted');
      if(typeof row.quotaUpdatedAt==='number')updated.title=this.time(row.quotaUpdatedAt);
      box.append(updated);
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
    if(this.notice){root.append(this.node('p',this.notice,this.noticeError?'error':'accounts-credit'));this.notice='';this.noticeError=false;}
    if(this.login)root.append(this.loginBox());
    if(value.captureError)root.append(this.node('p',t('当前登录态捕获失败：')+value.captureError,'account-muted'));
    const accounts=value.accounts||[];
    // 信封账号在下方卡片里已经写明了原因，这里不再重复同一条红字。
    const envelopeIds=new Set(accounts.filter(row=>row.tokenEnvelope).map(row=>row.id));
    if(value.refreshErrors)for(const[id,msg]of Object.entries(value.refreshErrors)){
      if(envelopeIds.has(id))continue;
      root.append(this.node('p',id+' · '+msg,'error'));
    }
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
      // 信封态账号不用删除再重加：直接走网关内扫码，登录完成后原地换掉密文凭据。
      if((row.status==='reauth_required'||row.tokenEnvelope)&&POOL_LOGIN[this.kind]){
        const reauth=this.button(t(row.tokenEnvelope?'扫码重新登录':'重新登录'),()=>this.openLogin(row.id,row.region));
        reauth.disabled=this.busy||this.loading;
        card.append(reauth);
      }
      // CodeBuddy 国内版：每日签到（当天已签则只显示状态，悬浮可见回执）。
      if(this.kind==='workbuddy'&&String(row.region||'').toLowerCase()!=='ai'){
        const checkedToday=row.checkinAt&&new Date(row.checkinAt).toDateString()===new Date().toDateString();
        if(checkedToday){
          const done=this.node('span',t('已签到'),'account-muted pool-checkin-done');
          done.title=row.checkinNote||'';
          card.append(done);
        }else{
          const checkinBtn=this.button(t('签到'),()=>this.checkin(row));
          checkinBtn.disabled=this.busy||this.loading;
          card.append(checkinBtn);
        }
      }
      // 国际版未开通：选地区 + 领 trial（14017 trial not activated 的出口）。
      if(this.kind==='workbuddy'&&String(row.region||'').toLowerCase()==='ai'&&row.aiProvision==='pending'){
        const wrap=this.node('div',undefined,'pool-provision');
        const select=document.createElement('select');
        select.setAttribute('aria-label',t('地区'));
        for(const region of WORKBUDDY_AI_REGIONS){
          const option=document.createElement('option');
          option.value=region.code;option.textContent=t(region.label);
          select.append(option);
        }
        const go=this.button(t('开通并领 Trial'),()=>this.provision(row,select.value));
        go.disabled=this.busy||this.loading;
        wrap.append(select,go);
        card.append(wrap);
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
