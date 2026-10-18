'use strict';
// Capture once and remove the bearer fragment before any API request or chat routing.
function takePairingToken(){
  if(!location.hash.startsWith('#pair='))return null;
  const token=location.hash.slice(6);
  history.replaceState(null,'',location.pathname+location.search);
  return token;
}
let initialPairingToken=takePairingToken();
const $ = id => document.getElementById(id),t=BridgeI18n.t;
let csrf='', currentId=null, state=null, listOffset=0, listQuery='', approvalStamp='', sending=false, autoQueueArmed=false;
let currentHost='local', listRows=[], listGeneration=0;
let chatTimeline=null;
// Keep the tab title on the open chat; user content is never passed through t().
function syncDocumentTitle(){
  const open=currentId&&!$('app').hidden&&$('app').classList.contains('chat-open');
  document.title=open&&state?.title&&!state.loadingHistory?state.title+' · '+t('Mobile / Bridge'):t('Mobile / Bridge · 手机连接');
}
const submittedRequestIds=new Set();
const sessionActivity=new SessionActivity();
const recentInteractions=new Map();
function bumpRecency(id,host,at=Date.now()){
  const key=chatKey(id,host);recentInteractions.set(key,Math.max(at,recentInteractions.get(key)||0));
  const row=listRows.find(row=>row.id===id&&row.host===host);
  if(!row||at<=row.recency)return;
  const before=listRows.indexOf(row);
  row.recency=at;
  const sorted=[...listRows].sort((a,b)=>b.recency-a.recency);
  if(sorted[before]===row){
    // The row keeps its position: refresh just its timestamp so an active
    // chat does not re-render (and scroll-jump) the sidebar every 5s.
    const time=document.querySelector('.session[data-id="'+id+'"][data-host="'+host+'"] time');
    if(time)time.textContent=dateText(at/1000);
    return;
  }
  listRows=sorted;renderList();
}
let activityBusy=false,listRevision=null;
const fastModeControl=new FastModeControl({root:$('fast-mode-control'),input:$('fast-mode-enabled'),status:$('fast-mode-status')});
let modelRequest=0,modelTarget=null,modelSaving=false;
async function goalAction(action,payload) {
  const id=currentId,host=currentHost;
  const result=await api(sessionUrl(id,'goal/'+action,host),payload);
  if(!result.confirmed)throw Error('目标操作结果尚未确认，请查看目标状态');
  if(currentId===id&&currentHost===host){state={...state,goal:action==='cancel'?null:(result.result?.goal||result.result)};workModes.render(state,$('send-mode').value,false);}
  toast(action==='cancel'?'目标已关闭':action==='edit'?'目标已修改，保持暂停':payload.status==='paused'?'目标已暂停':'目标已恢复');
}
const workModes=new WorkModes({select:$('work-mode'),hint:$('mode-hint'),goalRoot:$('goal-state'),goalToggle:$('goal-toggle'),newId:uuid,
  onCancel:payload=>goalAction('cancel',payload),onStatus:payload=>goalAction('status',payload),onEdit:payload=>goalAction('edit',payload)});
const accountPanel=new AccountPanel({root:$('account-content'),button:$('account-button'),read:refresh=>api('/api/account'+(refresh?'':'?cached=1')),consume:body=>api('/api/account/reset',body),onHidden:()=>{$('account-details').open=false;},visible:()=>!$('app').hidden&&!document.hidden&&$('accounts-dialog').open&&$('account-details').open});
$('account-details').ontoggle=()=>{if($('account-details').open)accountPanel.refresh();};
const migrateControl=typeof MigrateControl==='undefined'?null:new MigrateControl({root:$('migrate-control'),
  request:body=>api(sessionUrl(currentId,'migrate',currentHost),body),
  confirm:message=>window.confirm(message),
  onMigrated:async(result,label,body)=>{$('chat-details-dialog').close();
    if(!result.sent){
      try{
        const sourceId=currentId,sourceHost=currentHost;
        await openChat(result.id,result.host||'local');
        await new Promise(resolve=>setTimeout(resolve,800));
        result=await api(sessionUrl(sourceId,'migrate',sourceHost),body);
      }catch(error){result.warning=error.message;}
    }
    toast(result.sent?t('已迁移到 ')+label+t('，正在打开新会话'):t('目标会话已创建，正在等待 ')+label+t(' 加载：')+(result.warning||''));
    if(currentId!==result.id)openChat(result.id,result.host||'local').catch(e=>toast(e.message));},
  onError:e=>toast(e.message)});
window.addEventListener('focus',()=>{if(!$('app').hidden)accountsPanel?.refresh();});
function chatKey(id=currentId,host=currentHost){return host+'|'+id;}
function sessionUrl(id,action='',host=currentHost){return '/api/sessions/'+id+(action?'/'+action:'')+'?host='+encodeURIComponent(host);}
function hostUrl(path,host=currentHost){return /^\/api\/sessions\/[\w.-]+/.test(path)&&!/[?&]host=/.test(path)?path+(path.includes('?')?'&':'?')+'host='+encodeURIComponent(host):path;}
let catalogData={models:null,skills:null};let catalogResults={models:null,skills:null};let catalogTasks={models:null,skills:null};let selectedSkills=new Set(),invalidSkills=new Set(),skillIndex=new Map(),skillTotal=0,skillPages=[],skillQuery={q:'',offset:0,limit:200},skillSearchTimer,skillRequest=0;
function el(tag,cls,text){const node=document.createElement(tag);if(cls)node.className=cls;if(text!==undefined)node.textContent=text;return node;}
function toast(text){text=t(text);$('toast').textContent=text;$('toast').hidden=false;clearTimeout(toast.timer);toast.timer=setTimeout(()=>$('toast').hidden=true,4200);}
async function api(path,body,signal){const options={signal,credentials:'same-origin',cache:'no-store',headers:{}};if(body!==undefined){options.method='POST';options.headers={'Content-Type':'application/json','X-CSRF-Token':csrf};options.body=JSON.stringify(body);}const response=await fetch(hostUrl(path),options);const data=await response.json().catch(()=>({}));if(!response.ok){if(response.status===401)showLogin();throw Object.assign(Error(data.error||t('请求失败')),{loginStatus:data.loginStatus});}return data;}
async function uploadAttachment(key,id,file){const separator=key.indexOf('|'),host=key.slice(0,separator),thread=key.slice(separator+1);const response=await fetch(sessionUrl(thread,'uploads',host)+'&id='+encodeURIComponent(id)+'&name='+encodeURIComponent(file.name),{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/octet-stream','X-CSRF-Token':csrf},body:file});const result=await response.json();if(!response.ok){if(response.status===401)showLogin();const error=Error(result.error||t('附件上传失败，请重试'));error.retryable=response.status>=500||response.status===429;throw error;}return result;}
async function uploadAttachmentWithRetry(key,id,file){let last;for(let attempt=0;attempt<3;attempt++){try{return await uploadAttachment(key,id,file);}catch(error){last=error;if(error.name==='AbortError'||error.retryable===false||attempt===2)throw error;await new Promise(resolve=>setTimeout(resolve,400*(attempt+1)));}}if(last instanceof TypeError)last.message=t('附件上传中断，请重试');throw last;}
const attachments=new ChatAttachments({root:$('attachment-list'),button:$('attach-button'),input:$('attachment-input'),paste:$('message'),drop:$('chat'),preview:(key,id)=>{const at=key.indexOf('|'),host=key.slice(0,at),thread=key.slice(at+1);return hostUrl(sessionUrl(thread,'uploads/'+encodeURIComponent(id)+'/preview',host));},onChange:()=>{if(currentId)syncSendButton();},thumbnail:async(key,id,file)=>{
  const separator=key.indexOf('|'),host=key.slice(0,separator),thread=key.slice(separator+1);
  const image=await attachments.thumbnailBlob(file);
  const response=await fetch(sessionUrl(thread,'uploads/'+encodeURIComponent(id)+'/thumb',host)+'&width='+image.width+'&height='+image.height,{method:'POST',credentials:'same-origin',headers:{'Content-Type':image.mime,'X-CSRF-Token':csrf},body:image.data});
  const result=await response.json();if(!response.ok){if(response.status===401)showLogin();throw Error(result.error||t('缩略图生成失败'));}return result;
},upload:uploadAttachmentWithRetry});
function visibleChat(){return currentId&&!document.hidden&&$('app').classList.contains('chat-open')?chatKey():null;}
function renderActivity(){for(const button of document.querySelectorAll('.session')){
  button.querySelector('.session-indicator')?.remove();const indicator=sessionActivity.indicator(chatKey(button.dataset.id,button.dataset.host));
  if(indicator){const dot=el('span','session-indicator '+indicator.kind);dot.title=t(indicator.label);dot.setAttribute('aria-label',t(indicator.label));button.querySelector('strong').prepend(dot);}
}}
async function refreshActivity(){
  if(activityBusy||$('app').hidden||document.hidden)return;activityBusy=true;
  const hosts={};for(const row of listRows)(hosts[row.host]??=[]).push(row.id);
  try{const result=await api('/api/activity',{hosts});
    if(!$('app').hidden){
      // 定时发现的列表版本变化（Agent 侧新开、重命名或删除会话）时自动重载列表；
      // 首次见到的版本只记录基线，不触发重载。空列表也继续轮询，好让第一个
      // 网页之外创建的会话被发现。
      if(result.listRevision!=null){
        if(listRevision===null)listRevision=result.listRevision;
        else if(result.listRevision!==listRevision){listRevision=result.listRevision;loadList(true).catch(e=>toast(e.message));}
      }
      sessionActivity.update(result.sessions,visibleChat());for(const row of result.sessions)if(row.recency)bumpRecency(row.id,row.host,row.recency);renderActivity();
    }
  }catch{}finally{activityBusy=false;}
}
setInterval(refreshActivity,5000);
window.addEventListener('focus',refreshActivity);
// 前台恢复三件套之一：回前台立即刷新当前会话与列表，不等下一个轮询周期
// 或退避重连。focus 与 visibilitychange 常成对触发，1 秒内只执行一次。
let foregroundWakeAt=0;
function wakeForeground(){
  if(document.hidden)return;
  const now=Date.now();
  if(now-foregroundWakeAt<1000)return;
  foregroundWakeAt=now;
  refreshActivity();
  BridgeTransport.wake?.();
  chatTimeline?.refill?.();
}
document.addEventListener('visibilitychange',()=>{if(!document.hidden)wakeForeground();});
// Cellular/Wi-Fi handoffs recover without a visibility change; the SSE can
// stay silently dead, so an online event must also wake the channel.
window.addEventListener('online',wakeForeground);
let loginStatus={attemptsRemaining:5,attemptLimit:5,blocked:false};
function renderLoginStatus(status=loginStatus){
  loginStatus=status||loginStatus;
  $('login-attempts').textContent=loginStatus.blocked?t('此 IP 已被封禁，请在电脑网关 App 的“登录设备”中解除。'):t('剩余尝试次数：')+loginStatus.attemptsRemaining+'/'+loginStatus.attemptLimit+t('；连续输错 5 次将封禁此 IP。');
  $('login-button').disabled=loginStatus.blocked;
}
function showLogin(passwordless=false,status){if(typeof accountsPanel!=='undefined')accountsPanel?.clear();accountPanel.clear();chatTimeline?.dispose();chatTimeline=null;document.querySelectorAll('dialog[open]').forEach(d=>d.close());$('app').hidden=true;$('login').hidden=false;$('credentials').hidden=passwordless;$('noauth').hidden=!passwordless;$('username').required=!passwordless;$('password').required=!passwordless;$('password').value='';$('remember-login').parentElement.hidden=passwordless;$('login-attempts').hidden=passwordless;renderLoginStatus(status);}
async function start(pairingToken=null){
  const auth=await api('/api/auth');
  if(pairingToken!==null){
    try{const result=await api('/api/pair',{token:pairingToken});csrf=result.csrf;await enter();return;}
    catch(error){
      if(auth.authenticated){csrf=auth.csrf;await enter();toast(error.message);}
      else{showLogin(auth.passwordless,auth.loginStatus);$('login-error').textContent=t(error.message);}
      return;
    }finally{pairingToken=null;}
  }
  if(!auth.authenticated){showLogin(auth.passwordless,auth.loginStatus);return;}csrf=auth.csrf;await enter();
}
async function enter(){$('login').hidden=true;$('app').hidden=false;accountsPanel?.refresh();const list=loadList(true).catch(e=>toast(e.message));const [id,host='local']=location.hash.slice(1).split('~');if(/^[\w.-]{1,128}$/.test(id))await openChat(id,decodeURIComponent(host));await list;}
$('login-form').addEventListener('submit',async event=>{event.preventDefault();if(loginStatus.blocked)return;$('login-button').disabled=true;$('login-error').textContent='';try{const result=await api('/api/login',{username:$('username').value,password:$('password').value,remember:$('remember-login').checked});renderLoginStatus({attemptsRemaining:5,attemptLimit:5,blocked:false});csrf=result.csrf;$('password').value='';await enter();}catch(error){$('login-error').textContent=t(error.message);renderLoginStatus(error.loginStatus);}finally{$('login-button').disabled=loginStatus.blocked;}});
$('logout').onclick=async()=>{await api('/api/logout',{});csrf='';state=null;currentId=null;listRevision=null;$('messages').replaceChildren();$('approvals').replaceChildren();$('queued').replaceChildren();$('sessions').replaceChildren();$('message').value='';sessionStorage.clear();attachments.reset();sessionActivity.clear();recentInteractions.clear();showLogin();};
function dateText(value){if(!value)return '';const date=new Date(value*1000),now=new Date();if(date.getFullYear()===now.getFullYear()&&date.getMonth()===now.getMonth()&&date.getDate()===now.getDate())return date.toLocaleTimeString(BridgeI18n.locale(),{hour:'2-digit',minute:'2-digit'});return date.toLocaleDateString(BridgeI18n.locale(),{month:'numeric',day:'numeric'});}
$('list-mode').value=localStorage.getItem('list-mode')||'recent';
$('list-mode').onchange=()=>{localStorage.setItem('list-mode',$('list-mode').value);renderList();};
// 最近交互视图按最后对话时间分段折叠；一周内的分段默认展开，更早的默认收起。
const TIME_BUCKETS=[{days:1,label:'24 小时内',open:true},{days:3,label:'3 天内',open:true},{days:7,label:'一周内',open:true},{days:15,label:'15 天内',open:false},{days:30,label:'一月内',open:false},{days:90,label:'三月内',open:false},{days:Infinity,label:'更早',open:false}];
function renderList(){
  $('sessions').replaceChildren();
  const grouped=$('list-mode').value==='project', groups=new Map();
  const now=Date.now();
  for(const chat of listRows){
    let parent=$('sessions');
    if(grouped){
      if(!groups.has(chat.projectKey)){
        const details=el('details','project-group');
        details.open=localStorage.getItem('group:'+chat.projectKey)!=='closed';
        details.append(el('summary','',chat.projectName+' · '+(chat.host==='local'?t('此电脑'):chat.hostLabel)));
        details.ontoggle=()=>localStorage.setItem('group:'+chat.projectKey,details.open?'open':'closed');
        groups.set(chat.projectKey,details);$('sessions').append(details);
      }
      parent=groups.get(chat.projectKey);
    }else{
      const days=(now-(chat.recency||0))/864e5;
      const bucket=TIME_BUCKETS.find(b=>days<=b.days)||TIME_BUCKETS[TIME_BUCKETS.length-1];
      if(!groups.has(bucket)){
        const details=el('details','project-group');
        const stored=localStorage.getItem('tgroup:'+bucket.label);
        details.open=stored===null?bucket.open:stored==='open';
        details.append(el('summary','',t(bucket.label)));
        details.ontoggle=()=>localStorage.setItem('tgroup:'+bucket.label,details.open?'open':'closed');
        groups.set(bucket,details);$('sessions').append(details);
      }
      parent=groups.get(bucket);
    }
    const button=el('button','session'+(chat.id===currentId&&chat.host===currentHost?' selected':''));
    button.dataset.id=chat.id;button.dataset.host=chat.host;
    button.append(el('strong','',chat.title));
    const meta=el('small');const metaText=el('span','',chat.projectName+' · '+(chat.host==='local'?t('此电脑'):chat.hostLabel));
    const agentTag=BridgeI18n.agentTag(chat.agent);if(agentTag)metaText.prepend(el('em','agent-badge',agentTag));
    meta.append(metaText,el('time','',dateText(chat.recency/1000)));
    button.append(meta);button.onclick=()=>openChat(chat.id,chat.host).catch(e=>toast(e.message));parent.append(button);
  }
  if(!listRows.length)$('sessions').append(el('p','muted',t('没有找到聊天。')));renderActivity();
}
async function loadList(reset=false){
  const generation=++listGeneration;
  if(reset){listOffset=0;listRows=[];}
  listQuery=$('search').value;
  const result=await api('/api/sessions?q='+encodeURIComponent(listQuery)+'&offset='+listOffset+'&archived='+$('archived').checked);
  if(generation!==listGeneration)return;
  const rows=new Map(listRows.map(row=>[chatKey(row.id,row.host),row]));
  for(const row of result.sessions){const key=chatKey(row.id,row.host);row.recency=Math.max(row.recency,recentInteractions.get(key)||0);rows.set(key,row);}
  listRows=[...rows.values()];listRows.sort((a,b)=>b.recency-a.recency);
  listOffset+=result.sessions.length;$('more').hidden=result.sessions.length<100;
  $('host-errors').textContent=(result.unavailableHosts||[]).map(h=>h.label+'：'+h.error).join('\n');
  $('host-errors').hidden=!(result.unavailableHosts||[]).length;renderList();refreshActivity();
}
$('search').oninput=()=>{clearTimeout(loadList.timer);loadList.timer=setTimeout(()=>loadList(true).catch(e=>toast(e.message)),300);};$('refresh').onclick=()=>loadList(true).catch(e=>toast(e.message));$('more').onclick=()=>loadList().catch(e=>toast(e.message));
$('archived').onchange=()=>loadList(true).catch(e=>toast(e.message));
let newChatProjects=[],newChatAgents=[],creatingChat=false,approvalBusy=0;
function projectLabel(project){const kind=project.agent||(project.key||'').split(':')[0];const tag=BridgeI18n.agentTag(kind);return (tag?tag+' · ':'')+project.name+' · '+(project.host==='local'?t('此电脑'):project.hostLabel);}
function dirLabel(project){const base=(project.cwd||'').replace(/\\/g,'/').replace(/\/+$/,'').split('/').pop()||project.name;return base+' · '+(project.host==='local'?t('此电脑'):project.hostLabel);}
// 置顶项目存在 localStorage（按 host+cwd 识别，上限 20 个），渲染时排在最前并带 ★。
function newChatPins(){try{const pins=JSON.parse(localStorage.getItem('new-chat-pins')||'[]');return Array.isArray(pins)?pins:[];}catch{return[];}}
function saveNewChatPins(pins){localStorage.setItem('new-chat-pins',JSON.stringify(pins.slice(0,20)));}
function projectPinned(project){return newChatPins().some(pin=>pin.cwd===project.cwd&&pin.host===project.host);}
function newChatOptionLabel(project){return newChatAgents.length>1?dirLabel(project):projectLabel(project);}
function renderNewChatProjects(){
  const select=$('new-chat-project'),previous=select.value,filter=$('new-chat-filter').value.trim().toLowerCase();
  select.replaceChildren();
  const pinned=[],rest=[];
  for(const project of newChatProjects){
    if(filter&&!(newChatOptionLabel(project)+' '+(project.cwd||'')).toLowerCase().includes(filter))continue;
    (projectPinned(project)?pinned:rest).push(project);
  }
  for(const project of pinned)select.append(new Option('★ '+newChatOptionLabel(project),project.key));
  for(const project of rest)select.append(new Option(newChatOptionLabel(project),project.key));
  if([...select.options].some(o=>o.value===previous))select.value=previous;
  updateNewChatPin();
}
function updateNewChatPin(){
  const project=newChatProjects.find(p=>p.key===$('new-chat-project').value);
  const pinned=!!project&&projectPinned(project);
  $('new-chat-pin').textContent=pinned?'★':'☆';$('new-chat-pin').classList.toggle('active',pinned);
  $('new-chat-pin').title=t(pinned?'取消置顶此项目':'置顶此项目');
}
$('new-chat-filter').oninput=renderNewChatProjects;
$('new-chat-pin').onclick=()=>{
  const project=newChatProjects.find(p=>p.key===$('new-chat-project').value);if(!project)return;
  let pins=newChatPins();
  if(projectPinned(project))pins=pins.filter(pin=>!(pin.cwd===project.cwd&&pin.host===project.host));
  else pins.unshift({key:project.key,cwd:project.cwd,name:project.name,host:project.host,hostLabel:project.hostLabel,agent:project.agent});
  saveNewChatPins(pins);renderNewChatProjects();
};
// Codex chats open in the desktop App; ACP agents are spawned by the gateway.
function updateNewChatDesc(){const kind=(newChatAgents.length>1?$('new-chat-agent').value:newChatAgents[0]?.kind)||'codex';$('new-chat-desc').textContent=t(kind==='codex'?'在电脑 App 的已有项目中创建空聊天，之后可在手机发送消息。电脑会打开这条聊天。':'在已有项目目录中创建空聊天，之后可在手机发送消息。网关会直接启动该 Agent 的会话。');}
$('new-chat-title').value=t('新聊天');
$('new-chat-title').oninput=()=>{$('new-chat-title').dataset.edited='true';};
$('new-chat').onclick=async()=>{
  $('new-chat-error').textContent='';$('new-chat-submit').disabled=true;
  $('new-chat-worktree-row').hidden=true;$('new-chat-worktree-base-row').hidden=true;
  $('new-chat-project').replaceChildren();$('new-chat-path').textContent=t('正在读取电脑项目…');
  $('new-chat-dialog').showModal();
  try{
    const data=await api('/api/projects');
    newChatProjects=data.projects||[];newChatAgents=(data.agents||[]).filter(a=>a&&a.kind);
    const multi=newChatAgents.length>1;
    $('new-chat-agent-row').hidden=!multi;
    const agentSelect=$('new-chat-agent');agentSelect.replaceChildren();
    if(multi)for(const agent of newChatAgents)agentSelect.append(new Option(agent.label,agent.kind));
    // 多 Agent 时项目按目录去重：同目录跨 Agent 只显示一条，Agent 由上级选择器表达。
    const options=[],seen=new Set();
    for(const project of newChatProjects){
      if(multi){const cwd=project.cwd||'';if(seen.has(cwd))continue;seen.add(cwd);}
      options.push(project);
    }
    const current=listRows.find(row=>row.id===currentId&&row.host===currentHost);
    // 置顶项目和当前聊天项目即使掉出最近列表也保留入口
    const pins=newChatPins();
    for(let i=pins.length-1;i>=0;i--){
      const pin=pins[i];
      if(pin?.cwd&&!options.some(p=>p.cwd===pin.cwd&&p.host===pin.host))options.unshift(pin);
    }
    if(current?.projectKey&&current?.cwd&&!options.some(p=>p.key===current.projectKey||p.cwd===current.cwd))
      options.unshift({key:current.projectKey,cwd:current.cwd,name:current.projectName||(current.cwd.replace(/\\/g,'/').replace(/\/+$/,'').split('/').pop()||''),host:current.host,hostLabel:current.hostLabel,agent:current.agent});
    newChatProjects=options;
    $('new-chat-filter').value='';renderNewChatProjects();
    if(multi){
      if(newChatAgents.some(a=>a.kind===current?.agent))agentSelect.value=current.agent;
      else if(newChatAgents.some(a=>a.kind==='codex'))agentSelect.value='codex';
    }
    const selected=current?.projectKey;
    if(options.some(p=>p.key===selected))$('new-chat-project').value=selected;
    else if(current?.cwd){const hit=options.find(p=>p.cwd===current.cwd);if(hit)$('new-chat-project').value=hit.key;}
    $('new-chat-worktree').checked=localStorage.getItem('new-chat-worktree')==='1';
    $('new-chat-worktree-base').value=localStorage.getItem('new-chat-worktree-base')||'fresh';
    $('new-chat-project').onchange();updateNewChatDesc();
    if(!options.length)$('new-chat-error').textContent=t('请先在对应 Agent 的客户端添加一个项目。');
    $('new-chat-submit').disabled=!options.length;
  }catch(e){$('new-chat-error').textContent=t(e.message);$('new-chat-path').textContent='';}
};
function updateWorktreeRow(){
  const project=newChatProjects.find(p=>p.key===$('new-chat-project').value);
  const local=!!project&&project.host==='local';
  $('new-chat-worktree-row').hidden=!local;
  if(!local)$('new-chat-worktree').checked=false;  // SSH 项目不支持 worktree，隐藏并取消勾选
  $('new-chat-worktree-base-row').hidden=!local||!$('new-chat-worktree').checked;
}
$('new-chat-project').onchange=()=>{$('new-chat-path').textContent=newChatProjects.find(p=>p.key===$('new-chat-project').value)?.cwd||'';updateWorktreeRow();updateNewChatPin();};
$('new-chat-worktree').onchange=()=>{localStorage.setItem('new-chat-worktree',$('new-chat-worktree').checked?'1':'0');updateWorktreeRow();};
$('new-chat-worktree-base').onchange=()=>localStorage.setItem('new-chat-worktree-base',$('new-chat-worktree-base').value);
$('new-chat-agent').onchange=updateNewChatDesc;
$('new-chat-dialog').addEventListener('cancel',event=>{if(creatingChat)event.preventDefault();});
$('new-chat-form').onsubmit=async event=>{
  event.preventDefault();if(creatingChat)return;
  const project=$('new-chat-project').value,title=$('new-chat-title').value.trim(),agent=newChatAgents.length>1?$('new-chat-agent').value:null;
  const worktree=$('new-chat-worktree').checked&&!$('new-chat-worktree-row').hidden?{base:$('new-chat-worktree-base').value}:null;
  if(!project||!title){$('new-chat-error').textContent=t('请选择项目并填写聊天名称。');return;}
  let attempt;
  try{attempt=JSON.parse(sessionStorage.getItem('new-chat-attempt')||'null');}catch{}
  if(!attempt||attempt.project!==project||attempt.title!==title||attempt.agent!==agent||JSON.stringify(attempt.worktree||null)!==JSON.stringify(worktree))attempt={id:uuid(),project,title,...(agent?{agent}:{}),...(worktree?{worktree}:{})};
  sessionStorage.setItem('new-chat-attempt',JSON.stringify(attempt));
  creatingChat=true;setLanguageBusy();$('new-chat-error').textContent='';$('new-chat-submit').textContent=t('正在创建…');
  $('new-chat-form').querySelectorAll('input,select,button').forEach(n=>n.disabled=true);
  document.querySelector('[data-close="new-chat-dialog"]').disabled=true;
  try{
    const result=await api('/api/sessions',attempt);
    sessionStorage.removeItem('new-chat-attempt');$('new-chat-dialog').close();
    $('search').value='';$('archived').checked=false;
    loadList(true).catch(e=>toast(e.message));toast(result.message);
    await openChat(result.id,result.host);
  }catch(e){
    if($('new-chat-dialog').open)$('new-chat-error').textContent=t(e.message)+t('；若结果不确定，请先刷新列表检查，避免重复创建。');
    else toast(t('聊天已创建，连接暂未完成：')+t(e.message));
  }finally{
    creatingChat=false;setLanguageBusy();$('new-chat-submit').textContent=t('创建并打开');
    $('new-chat-form').querySelectorAll('input,select,button').forEach(n=>n.disabled=false);
    document.querySelector('[data-close="new-chat-dialog"]').disabled=false;
  }
};
const messageActions=new MessageActions({request:api,openChat,refresh:()=>loadList(true).catch(e=>toast(e.message)),uuid,notify:toast,busy:value=>{approvalBusy+=value?1:-1;setLanguageBusy();}});
document.addEventListener('bridge-message-error',event=>toast(event.detail));
$('fork-source').onclick=()=>{const source=state?.forkedFrom;if(source){$('chat-details-dialog').close();openChat(source.id,source.host).catch(e=>toast(e.message));}};
function saveDraft(){if(currentId)sessionStorage.setItem('draft:'+chatKey(),$('message').value);}
$('message').oninput=()=>{saveDraft();syncSendButton();};
async function openChat(id,host="local"){if(messageActions.pending&&messageActions.dialog.open)return;messageActions.dialog.close();saveDraft();chatTimeline?.dispose();currentId=id;currentHost=host;state=null;syncDocumentTitle();workModes.open(chatKey(id,host));attachments.open(chatKey(id,host));catalogData={models:null,skills:null};catalogResults={models:null,skills:null};catalogTasks={models:null,skills:null};skillIndex=new Map();skillTotal=0;skillPages=[];skillQuery={q:'',offset:0,limit:200};invalidSkills=new Set();skillRequest++;modelRequest++;modelTarget=null;fastModeControl.reset();$('model-dialog').close();notificationState={available:false,watching:false,notifyOnCompletion:false};notificationRequest++;$('notify-dialog').close();$('permission-dialog').close();permissionState={supported:false,options:[],current:null};renderPermissionState();$('rename-dialog').close();$('chat-details-dialog').close();$('migrate-control').hidden=true;$('notify-button').disabled=true;$('notify-button').textContent=t('提醒');try{selectedSkills=new Set(JSON.parse(sessionStorage.getItem('skills:'+chatKey(id,host))||'[]'));}catch{selectedSkills=new Set();}renderSkillPills();approvalStamp='';submittedRequestIds.clear();$('messages').replaceChildren();$('approvals').replaceChildren();$('queued').replaceChildren();$('message').value=sessionStorage.getItem('draft:'+chatKey(id,host))||'';$('send-error').textContent='';$('chat-title').textContent=t('正在连接…');$('chat-meta').textContent='';$('status').textContent=t('连接中');$('welcome').hidden=true;$('chat').hidden=false;$('app').classList.add('chat-open');syncSendButton();history.replaceState(null,'','#'+id+'~'+encodeURIComponent(host));document.querySelectorAll('.session').forEach(n=>n.classList.toggle('selected',n.dataset.id===id&&n.dataset.host===host));const timeline=chatTimeline=new ChatTimeline({url:action=>sessionUrl(id,action,host),request:api,renderMeta:renderState,renderText:(node,text,files,fullText)=>renderMarkdown(node,text,files,file=>sessionUrl(id,'files/'+file.id,host),{fullText}),onAction:(action,row,timeline)=>{const pendingKey='fork-edit:'+host+'|'+id;let pending;try{pending=JSON.parse(sessionStorage.getItem(pendingKey));}catch{}if(pending&&pending.sourceTurnId===row.turnId&&action==='edit'){sessionStorage.setItem('message-edit:'+timeline.url('message-action')+':'+row.key,pending.text);sessionStorage.removeItem(pendingKey);}return messageActions.open(action,row,timeline);},status:text=>{text==='unauthorized'?showLogin():$('status').textContent=t(text);}});
  window.BridgePresentation?.openChat();
  startCatalog();
  const reading=timeline.start();
  // Opening a chat activates its original owner while saved history paints.
  api(sessionUrl(id,'reconnect',host),{activate:true},timeline.abort.signal).catch(error=>{if(!timeline.abort.signal.aborted)toast(error.message);});
  await reading;
  if(!timeline.abort.signal.aborted){loadNotificationState(id,host);loadPermissionSettings(id,host);}
}
let permissionState={supported:false,options:[],current:null},permissionRequest=0;
function renderPermissionState(){
  const button=$('permission-button');
  button.hidden=!permissionState.supported;
  if(!permissionState.supported)return;
  const select=$('permission-select');select.replaceChildren();
  for(const option of permissionState.options||[])select.append(new Option(option.name||option.value,option.value));
  select.value=permissionState.current||'';
}
async function loadPermissionSettings(id=currentId,host=currentHost){
  const target=id,targetHost=host;
  // 请求版本：迟到的旧响应不得覆盖新响应（权限面板可能在打开中被重复加载）。
  const request=++permissionRequest;
  try{
    const result=await api(sessionUrl(target,'permissions',targetHost));
    if(request!==permissionRequest||target!==currentId||targetHost!==currentHost)return;
    permissionState=result;renderPermissionState();
  }catch(error){
    if(request===permissionRequest&&target===currentId&&targetHost===currentHost){permissionState={supported:false,options:[],current:null};renderPermissionState();}
  }
}
$('permission-button').onclick=async()=>{
  $('permission-error').textContent='';$('permission-dialog').showModal();
  await loadPermissionSettings();
  if($('permission-dialog').open&&permissionState.supported)$('permission-select').value=permissionState.current||'';
};
$('permission-form').onsubmit=async event=>{
  event.preventDefault();
  const id=currentId,host=currentHost,mode=$('permission-select').value;
  $('permission-save').disabled=true;$('permission-error').textContent='';
  try{
    permissionState=await api(sessionUrl(id,'permissions',host),{mode});
    renderPermissionState();$('permission-dialog').close();toast(t('权限已更新'));
  }catch(error){$('permission-error').textContent=t(error.message);}
  finally{$('permission-save').disabled=false;}
};
let notificationState={available:false,watching:false,notifyOnCompletion:false},notificationRequest=0;
function renderNotificationState(){$('notify-button').textContent=t(notificationState.watching?'提醒已开':'提醒');}
async function loadNotificationState(id=currentId,host=currentHost){
  const request=++notificationRequest;
  $('notify-button').disabled=true;
  try{const result=await api(sessionUrl(id,'notifications',host));if(request!==notificationRequest||id!==currentId||host!==currentHost)return;notificationState=result;renderNotificationState();return true;}
  catch(e){if(request===notificationRequest&&id===currentId&&host===currentHost){$('notify-error').textContent=t(e.message);notificationState={...notificationState,error:e.message};}return false;}
  finally{if(request===notificationRequest&&id===currentId&&host===currentHost)$('notify-button').disabled=false;}
}
$('notify-button').onclick=async()=>{
  const key=chatKey();
  $('notify-error').textContent='';$('notify-dialog').showModal();
  for(const id of ['notify-enabled','notify-completion','notify-save'])$(id).disabled=true;
  const loaded=await loadNotificationState();
  if(!$('notify-dialog').open||key!==chatKey()||!loaded)return;
  $('notify-enabled').value=notificationState.requests||'inherit';
  $('notify-completion').value=notificationState.completion||'inherit';
  for(const id of ['notify-enabled','notify-completion','notify-save'])$(id).disabled=false;
  $('notify-channel-status').textContent=t(notificationState.available?'通知通道已开启':'尚未配置通知通道，可在电脑端配置 Bark 或 ntfy。');
};
$('notify-form').onsubmit=async event=>{
  event.preventDefault();
  const id=currentId,host=currentHost,request=++notificationRequest;
  const body={requests:$('notify-enabled').value,completion:$('notify-completion').value};
  $('notify-button').disabled=true;$('notify-save').disabled=true;$('notify-error').textContent='';
  try{
    const result=await api(sessionUrl(id,'notifications',host),body);
    if(request!==notificationRequest||id!==currentId||host!==currentHost)return;
    notificationState=result;renderNotificationState();$('notify-dialog').close();toast('聊天提醒设置已保存');
  }catch(e){if(request===notificationRequest&&id===currentId&&host===currentHost)$('notify-error').textContent=t(e.message);}
  finally{if(request===notificationRequest&&id===currentId&&host===currentHost){$('notify-button').disabled=false;$('notify-save').disabled=false;}}
};
let notificationDefaultsRequest=0,notificationDirty=false;
window.loadNotificationDefaults=async()=>{
  const request=++notificationDefaultsRequest;
  for(const id of ['notification-requests','notification-completions'])$(id).disabled=true;
  $('notification-settings-error').textContent='';
  if($('app').hidden)return;
  try{
    const result=await api('/api/notifications/defaults');if(request!==notificationDefaultsRequest)return;
    $('notification-requests').checked=result.requests;$('notification-completions').checked=result.completion;
    for(const id of ['notification-requests','notification-completions'])$(id).disabled=false;
    notificationDirty=false;
  }catch(error){$('notification-settings-error').textContent=t(error.message);}
};
async function saveNotificationDefaults(){
  const body={requests:$('notification-requests').checked,completion:$('notification-completions').checked};
  for(const id of ['notification-requests','notification-completions'])$(id).disabled=true;
  try{await api('/api/notifications/defaults',body);if(currentId)await loadNotificationState();}
  catch(error){await window.loadNotificationDefaults();$('notification-settings-error').textContent=t(error.message);return false;}
  await window.loadNotificationDefaults();
  return true;
}
$('notification-requests').onchange=$('notification-completions').onchange=()=>{notificationDirty=true;};
let jevSettingsRequest=0,jevFormReady=false,jevDirty=false,jevClearPending=false;
window.loadJevSettings=async()=>{
  const request=++jevSettingsRequest;
  jevFormReady=false;
  $('jev-settings-error').textContent='';
  try{
    const result=await api('/api/jev');if(request!==jevSettingsRequest)return;
    $('jev-key').value='';
    const configured=Boolean(result.configured);
    // The capsule acts as the field's value: while it shows, the input is
    // closed for typing; clearing the capsule reopens the empty field.
    $('jev-key-pill').hidden=!configured;
    $('jev-key-field').classList.toggle('has-pill',configured);
    $('jev-key-pill-text').textContent=result.apiKeyMasked||t('已配置');
    $('jev-key').placeholder=configured?'':t('未配置');
    $('jev-key').readOnly=configured;
    $('jev-base-url').value=result.baseUrl;$('jev-model').value=result.model;$('jev-proxy').value=result.proxy||'';
    $('jev-auto-approve').checked=Boolean(result.autoApprove);
    $('jev-auto-continue').checked=Boolean(result.autoContinue);
    jevFormReady=true;
    jevDirty=false;jevClearPending=false;
  }catch(error){if(request===jevSettingsRequest)$('jev-settings-error').textContent=t(error.message);}
};
// Jev edits stage locally; the dialog's single 保存 button commits them.
async function saveJevChanges(){
  if(!jevFormReady||!jevDirty)return true;
  const body={baseUrl:$('jev-base-url').value.trim(),model:$('jev-model').value.trim(),proxy:$('jev-proxy').value.trim(),autoApprove:$('jev-auto-approve').checked,autoContinue:$('jev-auto-continue').checked};
  const key=$('jev-key').value.trim();
  if(key)body.apiKey=key;else if(jevClearPending)body.clearApiKey=true;
  try{await api('/api/jev',body);}
  catch(error){$('jev-settings-error').textContent=t(error.message);return false;}
  return true;
}
$('jev-key-remove').onclick=()=>{
  // Local only: reopening the field for a new key; 保存 commits the clear.
  $('jev-key-pill').hidden=true;
  $('jev-key-field').classList.remove('has-pill');
  $('jev-key').readOnly=false;$('jev-key').value='';$('jev-key').placeholder=t('未配置');
  jevClearPending=true;jevDirty=true;
};
['jev-auto-approve','jev-auto-continue','jev-base-url','jev-model','jev-proxy'].forEach(id=>$(id).onchange=()=>{jevDirty=true;});
$('jev-key').onchange=()=>{if($('jev-key').value.trim())jevDirty=true;};
let conventionsRequest=0,conventionsDirty=false;
window.loadConventions=async()=>{
  const request=++conventionsRequest;
  $('conventions-artifact-paths').disabled=true;
  $('conventions-error').textContent='';
  try{
    const result=await api('/api/conventions');if(request!==conventionsRequest)return;
    $('conventions-artifact-paths').checked=Boolean(result.artifactPaths);
    $('conventions-artifact-paths').disabled=false;
    conventionsDirty=false;
  }catch(error){if(request===conventionsRequest)$('conventions-error').textContent=t(error.message);}
};
// Conventions stage locally like every server-persisted item in this dialog.
async function saveConventions(){
  if(!conventionsDirty)return true;
  $('conventions-artifact-paths').disabled=true;
  try{
    await api('/api/conventions',{artifactPaths:$('conventions-artifact-paths').checked});
    $('conventions-artifact-paths').disabled=false;
    conventionsDirty=false;
    return true;
  }
  catch(error){$('conventions-error').textContent=t(error.message);await window.loadConventions();return false;}
}
$('conventions-artifact-paths').onchange=()=>{conventionsDirty=true;};
// The one save button for every server-persisted setting in this dialog;
// appearance stays browser-local and immediate.
$('settings-save').onclick=async()=>{
  const button=$('settings-save');button.disabled=true;
  let ok=true;
  if(await saveJevChanges()){jevDirty=false;jevClearPending=false;await window.loadJevSettings();}
  else ok=false;
  if(notificationDirty&&!await saveNotificationDefaults())ok=false;
  if(await saveConventions()){await window.loadConventions();}
  else ok=false;
  button.disabled=false;
  toast(ok?t('设置已保存'):t('部分设置保存失败，请检查表单错误'));
};
$('back').onclick=()=>{$('app').classList.remove('chat-open');history.replaceState(null,'',location.pathname);syncDocumentTitle();loadList(true).catch(e=>toast(e.message));};
function renderState(view){state=view;const caps=new Set(view.capabilities||[]);const readOnly=!!view.readOnly;const agentName=BridgeI18n.agentLabel(view.agent);$('fork-source').hidden=!view.forkedFrom;$('fork-source').textContent=view.forkedFrom?t('来源会话：')+view.forkedFrom.title:'';if($('model-dialog').open&&modelTarget===chatKey())fastModeControl.sync(view);if(visibleChat()){sessionActivity.read(chatKey());renderActivity();}$('execution-host').textContent=t('在 ')+(view.host==='local'?t('电脑'):view.hostLabel||t('SSH 主机'))+t(' 运行');$('chat-title').textContent=view.loadingHistory?t('正在读取聊天…'):view.title;$('details-title').textContent=$('chat-title').textContent;$('chat-meta').textContent=(BridgeI18n.agentTag(view.agent)?BridgeI18n.agentTag(view.agent)+' · ':'')+(view.host==='local'?t('此电脑'):view.hostLabel||t('SSH 主机'))+' · '+(view.cwd||t('电脑上的聊天'));$('model-name').textContent=view.model||t('模型');$('model-current').textContent=view.model?t('当前模型：')+view.model:'';$('model-current').hidden=!view.model;$('model-effort').textContent=view.effort?'· '+view.effort:'';$('model-effort').hidden=!view.effort;$('model-button').title=[view.model||t('模型'),view.effort].filter(Boolean).join(' · ');$('provider').textContent=[view.model,view.provider].filter(Boolean).join(' · ');$('status').textContent=!view.connected?(view.loadingHistory?t('读取历史中'):view.activating?(view.agent==='codex'?t('正在加载桌面聊天…'):t('正在加载会话…')):view.connecting?(view.agent==='codex'?t('连接桌面中'):t('正在连接 ')+agentName):t('历史记录')):view.requests.length?t('等待回应'):view.status==='active'?t('运行中'):t('已连接');$('status').classList.toggle('offline',!view.connected);if(readOnly)$('status').textContent=t('只读');$('notice').hidden=view.connected;$('notice-text').textContent=view.activating?(view.agent==='codex'?t('正在自动连接；必要时会在电脑 Codex 中打开此聊天。'):t('正在加载会话，请稍候。')):view.connecting?(view.agent==='codex'?t('连接桌面中'):t('正在连接 ')+agentName+t(' ，可先阅读历史记录。')):t(view.connectionError)||t('暂未连接，可先阅读历史或点击重新连接。');const workingSpan=$('working').querySelector('span');if(workingSpan)workingSpan.textContent=agentName+t(' 正在工作…');$('history').hidden=view.historyComplete||!view.connected;$('working').hidden=view.status!=='active';$('stop').hidden=view.status!=='active'||!view.connected||readOnly;syncSendButton();const steerOption=$('send-mode').querySelector('[value=steer]');steerOption.hidden=!caps.has('steer');steerOption.disabled=!caps.has('steer')||view.status!=='active';if((!caps.has('steer')||view.status!=='active')&&$('send-mode').value==='steer')$('send-mode').value='send';
  // Agents without steering only have queueing while busy: preselect it once
  // per run so one tap on 发送 queues the message instead of erroring out.
  if(view.status==='active'&&!caps.has('steer')&&$('send-mode').value==='send'&&!autoQueueArmed){autoQueueArmed=true;$('send-mode').value='queue';}
  if(autoQueueArmed&&view.status!=='active'){autoQueueArmed=false;if($('send-mode').value==='queue')$('send-mode').value='send';}$('skills-button').hidden=!caps.has('skills')||readOnly;renderPermissionState();$('rename-chat').hidden=!caps.has('rename');if(!caps.has('skills')&&selectedSkills.size){selectedSkills.clear();renderSkillPills();}workModes.render(view,$('send-mode').value,sending);$('message').readOnly=readOnly;$('readonly-hint').hidden=!readOnly;$('readonly-text').textContent=readOnly?t('此会话正在桌面 App 中运行，手机端只读；可对消息点「从这里分支」在手机继续；「尝试接入」前请先关闭桌面端该会话'):'';$('send-mode').hidden=readOnly;$('work-mode').hidden=readOnly;$('attach-button').hidden=readOnly;$('model-button').hidden=readOnly;$('readonly-upgrade').disabled=readOnly&&view.status==='active';$('readonly-upgrade').title=readOnly&&view.status==='active'?t('桌面端正在运行回合；结束后再试'):'';$('composer').classList.toggle('read-only',readOnly);migrateControl?.render(view);renderApprovals(view.requests);renderQueue(view.submissions||[]);syncDocumentTitle();}
$('send-mode').onchange=()=>{autoQueueArmed=false;workModes.render(state,$('send-mode').value,sending);};
function jsonPretty(value){return typeof value==='string'?value:JSON.stringify(value,null,2);}
function approvalRisk(params){
  const command=String(params?.command||'').toLowerCase();
  if(params?.networkApprovalContext||/\b(curl|wget|ssh|scp|rsync|nc|netcat)\b/.test(command))return ['network','网络访问'];
  if(/\b(sudo|rm\s+-rf|chmod\s+777|chown)\b/.test(command))return ['high','高风险操作'];
  if(params?.changes||params?.grantRoot)return ['file','文件修改'];
  if(command)return ['command','命令执行'];
  return null;
}
function assessmentBadge(request){
  const a=request?.assessment;
  if(!a||a.source!=='jev')return null;
  const labels={routine:'常规操作',low:'低风险',elevated:'中风险',high:'高风险'};
  const label=labels[a.riskLabel];
  if(!label)return null;
  let text=t(label)+' · Jev';
  if(a.confidence!=null)text+=' '+Math.round(a.confidence*100)+'%';
  if(a.reversible!=null)text+=a.reversible>=0.5?' · '+t('可撤销'):' · '+t('难以撤销');
  return el('p','approval-risk risk-'+a.riskLabel,text);
}
function riskBadge(request,params){
  const fallback=approvalRisk(params);
  return assessmentBadge(request)||(fallback?el('p','approval-risk '+fallback[0],t(fallback[1])):null);
}
function markRecommendation(button,value,recommend){
  if(!recommend||value!==recommend)return;
  button.classList.add('jev-recommend');
  button.title=t('Jev 建议此选项');
  button.prepend(document.createTextNode('★ '));
}
function renderApprovals(requests){const stamp=JSON.stringify(requests);if(stamp===approvalStamp)return;approvalStamp=stamp;$('approvals').replaceChildren();for(const request of requests){if(request.method==='acp/requestInput'&&request.supported){$('approvals').append(renderAcpInput(request));continue;}if(request.method==='acp/requestPermission'&&request.supported){$('approvals').append(renderAcpApproval(request));continue;}if(request.supported&&request.params?.computerUse){$('approvals').append(renderComputerApproval(request));continue;}if(request.method==='item/plan/requestImplementation'&&request.supported){$('approvals').append(renderPlanApproval(request));continue;}const card=el('section','approval');card.dataset.request=String(request.id);const params=request.params||{}, method=request.method;const heading=method.includes('requestUserInput')?t('Codex 需要你的回复'):method.includes('fileChange')?t('允许修改文件？'):method.includes('commandExecution')?t('允许运行此命令？'):method.includes('permissions')?t('允许本轮权限？'):t('待确认请求');card.append(el('h3','',heading));const controls={};if(!request.supported){card.append(el('p','',params.message||t('请在桌面 App 处理此请求')));$('approvals').append(card);continue;}const badge=riskBadge(request,params);if(badge)card.append(badge);if(params.reason)card.append(el('p','',params.reason));if(params.command){const command=el('pre','approval-command',jsonPretty(params.command));command.setAttribute('aria-label',t('具体命令'));card.append(command);}if(params.cwd)card.append(el('p','approval-context',t('工作目录：')+params.cwd));for(const key of ['changes','grantRoot','permissions','networkApprovalContext'])if(params[key])card.append(el('pre','approval-details',jsonPretty(params[key])));const form=el('form');if(method.includes('requestUserInput')){for(const question of params.questions||[]){const label=el('label','',question.question||question.header||question.id);if(question.options?.length){const select=el('select');select.append(new Option(t('请选择…'),''));for(const option of question.options)select.append(new Option(option.label+(option.description?' — '+option.description:''),option.label));select.append(new Option(t('自行填写'),'__custom__'));label.append(select);const custom=el('textarea');custom.rows=2;custom.hidden=true;custom.placeholder=t('输入你的回复');select.onchange=()=>custom.hidden=select.value!=='__custom__';label.append(custom);controls[question.id]=()=>select.value==='__custom__'?custom.value:select.value;}else{const input=el('textarea');input.rows=2;input.required=true;label.append(input);controls[question.id]=()=>input.value;}form.append(label);}}else if(method==='mcpServer/elicitation/request'){if(params.message)form.append(el('p','',params.message));const schema=params.requestedSchema||{};for(const [key,field] of Object.entries(schema.properties||{})){const label=el('label','',field.title||key);let input;if(field.enum){input=el('select');input.append(new Option(t('请选择…'),''));for(const [i,value] of field.enum.entries())input.append(new Option(field.enumNames?.[i]||String(value),String(value)));}else{input=el('input');input.type=field.type==='boolean'?'checkbox':['number','integer'].includes(field.type)?'number':'text';if(input.type==='number')input.step=field.type==='integer'?'1':'any';}input.required=(schema.required||[]).includes(key)&&input.type!=='checkbox';if(field.description)label.append(el('p','muted',field.description));label.append(input);form.append(label);controls[key]=()=>input.type==='checkbox'?input.checked:input.value===''?undefined:['number','integer'].includes(field.type)?Number(input.value):input.value;}}const actions=el('div','actions'),error=el('p','error');async function submit(response){const id=currentId,host=currentHost;approvalBusy++;setLanguageBusy();const buttons=card.querySelectorAll('button');buttons.forEach(b=>b.disabled=true);error.textContent='';try{await api(sessionUrl(id,'respond',host),{requestId:request.id,response});submittedRequestIds.add(request.id);bumpRecency(id,host);toast('已发送回应');card.append(el('p','muted',t('已发送，等待桌面确认…')));}catch(e){error.textContent=t(e.message);buttons.forEach(b=>b.disabled=false);}finally{approvalBusy--;setLanguageBusy();}}const isInput=method.includes('requestUserInput'),isMcp=method==='mcpServer/elicitation/request';const yes=el('button','primary',isInput?t('提交回复'):t('本次允许'));markRecommendation(yes,'accept',request.recommend);yes.type='submit';const available=params.availableDecisions;const canAccept=!available||available.includes('accept');if(isInput||isMcp||canAccept)actions.append(yes);if(!isInput){const no=el('button','secondary',t('拒绝'));markRecommendation(no,'decline',request.recommend);no.type='button';no.onclick=()=>submit(isMcp?{action:'decline'}:{decision:'decline'});if(!available||available.includes('decline'))actions.append(no);if(method.includes('commandExecution')||method.includes('fileChange')||isMcp){const cancel=el('button','secondary',t('取消'));cancel.type='button';cancel.onclick=()=>submit(isMcp?{action:'cancel'}:{decision:'cancel'});if(!available||available.includes('cancel'))actions.append(cancel);}}form.onsubmit=event=>{event.preventDefault();if(isInput){const answers={};for(const [key,get] of Object.entries(controls)){const value=get();if(!value.trim()){error.textContent=t('请回答所有问题');return;}answers[key]=[value];}submit({answers});}else if(isMcp){const content={};for(const [key,get] of Object.entries(controls)){const value=get();if(value!==undefined)content[key]=value;}submit({action:'accept',content});}else submit({decision:'accept'});};form.append(actions,error);card.append(form);if(submittedRequestIds.has(request.id)){card.querySelectorAll('button').forEach(button=>button.disabled=true);card.append(el('p','muted',t('已发送，等待桌面确认…')));}$('approvals').append(card);}}
function renderComputerApproval(request){
  const id=currentId,host=currentHost,card=el('section','approval'),computer=request.params.computerUse;
  card.dataset.request=String(request.id);card.append(el('h3','',t('允许使用电脑应用？')),el('p','',computer.app));
  if(request.params.message)card.append(el('p','',request.params.message));
  const badge=riskBadge(request,{});if(badge)card.append(badge);
  const actions=el('div','actions'),error=el('p','error');card.append(actions,error);
  let busy=submittedRequestIds.has(request.id);
  async function submit(response){
    if(busy)return;busy=true;approvalBusy++;setLanguageBusy();actions.querySelectorAll('button').forEach(b=>b.disabled=true);error.textContent='';
    try{await api(sessionUrl(id,'respond',host),{requestId:request.id,response});bumpRecency(id,host);if(id===currentId&&host===currentHost)submittedRequestIds.add(request.id);card.append(el('p','muted',t('已发送，等待桌面确认…')));}
    catch(e){error.textContent=t(e.message);busy=false;actions.querySelectorAll('button').forEach(b=>b.disabled=false);}
    finally{approvalBusy--;setLanguageBusy();}
  }
  function choice(label,response,tag,primary=false){const button=el('button',primary?'primary':'secondary',t(label));markRecommendation(button,tag,request.recommend);button.type='button';button.disabled=busy;button.onclick=()=>submit(response);actions.append(button);}
  if(computer.persistModes.includes('always'))choice('始终允许',{action:'accept',persist:'always'},'always');
  choice('拒绝',{action:'decline'},'decline');
  if(computer.persistModes.includes('session'))choice('允许此对话',{action:'accept',persist:'session'},'session',true);
  else choice('本次允许',{action:'accept'},'once',true);
  return card;
}
function renderAcpApproval(request){
  const id=currentId,host=currentHost,card=el('section','approval'),params=request.params||{};
  // kimi's AskUserQuestion arrives as a permission request whose options are
  // the answer choices: draw it as a question card, not a risk prompt.
  const question=!!params.question;
  card.dataset.request=String(request.id);card.append(el('h3','',question?t('Agent 提问你'):t('权限请求')),el('p','',params.text||(params.title||'')));
  const badge=question?null:riskBadge(request,params);if(badge)card.append(badge);
  if(!question&&params.command){const command=el('pre','approval-command',params.command);command.setAttribute('aria-label',t('具体命令'));card.append(command);}
  const actions=el('div','actions'),error=el('p','error');card.append(actions,error);
  let busy=submittedRequestIds.has(request.id);
  async function submit(response){
    if(busy)return;busy=true;approvalBusy++;setLanguageBusy();actions.querySelectorAll('button').forEach(b=>b.disabled=true);error.textContent='';
    try{await api(sessionUrl(id,'respond',host),{requestId:request.id,response});bumpRecency(id,host);if(id===currentId&&host===currentHost)submittedRequestIds.add(request.id);card.append(el('p','muted',t('已发送，等待桌面确认…')));}
    catch(e){error.textContent=t(e.message);busy=false;actions.querySelectorAll('button').forEach(b=>b.disabled=false);}
    finally{approvalBusy--;setLanguageBusy();}
  }
  let firstAllow=true;
  for(const option of params.options||[]){
    const allow=String(option.kind||'').includes('allow');
    const label=question&&!allow&&/^skip$/i.test(option.label||'')?t('跳过'):(option.label||option.id);
    const button=el('button',allow&&firstAllow?'primary':'secondary',label);
    if(allow)firstAllow=false;
    markRecommendation(button,option.id,request.recommend);
    button.type='button';button.disabled=busy;button.onclick=()=>submit({optionId:option.id});actions.append(button);
  }
  const cancel=el('button','secondary',t('取消'));cancel.type='button';cancel.disabled=busy;cancel.onclick=()=>submit({cancel:true});actions.append(cancel);
  if(busy)card.append(el('p','muted',t('已发送，等待桌面确认…')));
  return card;
}
function renderAcpInput(request){
  // ACP elicitation form: the agent (AskUserQuestion-style) asks the user to
  // answer structured questions; every wrapper field type maps to one control.
  const id=currentId,host=currentHost,card=el('section','approval'),params=request.params||{};
  card.dataset.request=String(request.id);card.append(el('h3','',t('Agent 需要你的回复')));
  if(params.message)card.append(el('p','',params.message));
  const schema=params.schema||{},required=new Set(schema.required||[]),controls={};
  const form=el('form');
  for(const [key,field] of Object.entries(schema.properties||{})){
    const label=el('label','',field.title||field.description||key);
    if(field.title&&field.description)label.append(el('p','muted',field.description));
    const variants=Array.isArray(field.oneOf)?field.oneOf:Array.isArray(field.enum)?field.enum.map(value=>({const:value,title:String(value)})):field.type==='array'&&Array.isArray(field.items?.anyOf)?field.items.anyOf:null;
    let input;
    if(field.type==='array'&&variants){
      input=el('div','choice-list');const boxes=[];
      for(const option of variants){
        const value=String(option.const??option),row=el('label','',option.title||value),box=el('input');
        box.type='checkbox';box.value=value;row.prepend(box);input.append(row);boxes.push(box);
      }
      controls[key]=()=>boxes.filter(box=>box.checked).map(box=>box.value);
    }else if(variants){
      input=el('select');input.append(new Option(t('请选择…'),''));
      for(const option of variants)input.append(new Option(option.title||String(option.const??option),String(option.const??option)));
      controls[key]=()=>input.value;
    }else{
      input=el('input');input.type=field.type==='boolean'?'checkbox':['number','integer'].includes(field.type)?'number':'text';
      if(input.type==='number')input.step=field.type==='integer'?'1':'any';
      controls[key]=()=>input.type==='checkbox'?input.checked:input.value===''?undefined:['number','integer'].includes(field.type)?Number(input.value):input.value;
    }
    input.required=required.has(key)&&input.type!=='checkbox';
    label.append(input);form.append(label);
  }
  const actions=el('div','actions'),error=el('p','error');
  let busy=submittedRequestIds.has(request.id);
  async function submit(response){
    if(busy)return;busy=true;approvalBusy++;setLanguageBusy();actions.querySelectorAll('button').forEach(b=>b.disabled=true);error.textContent='';
    try{await api(sessionUrl(id,'respond',host),{requestId:request.id,response});bumpRecency(id,host);if(id===currentId&&host===currentHost)submittedRequestIds.add(request.id);card.append(el('p','muted',t('已发送，等待桌面确认…')));}
    catch(e){error.textContent=t(e.message);busy=false;actions.querySelectorAll('button').forEach(b=>b.disabled=false);}
    finally{approvalBusy--;setLanguageBusy();}
  }
  const yes=el('button','primary',t('提交回答'));yes.type='submit';yes.disabled=busy;
  const no=el('button','secondary',t('拒绝'));no.type='button';no.disabled=busy;no.onclick=()=>submit({action:'decline'});
  const cancel=el('button','secondary',t('取消'));cancel.type='button';cancel.disabled=busy;cancel.onclick=()=>submit({action:'cancel'});
  actions.append(yes,no,cancel);
  form.onsubmit=event=>{event.preventDefault();const content={};for(const [key,get] of Object.entries(controls)){const value=get();if(value===undefined||value===''||(Array.isArray(value)&&!value.length)){if(required.has(key)){error.textContent=t('请填写所有必填项');return;}continue;}content[key]=value;}submit({action:'accept',content});};
  form.append(actions,error);card.append(form);
  if(busy)card.append(el('p','muted',t('已发送，等待桌面确认…')));
  return card;
}
function renderPlanApproval(request){
  const id=currentId,host=currentHost,key=chatKey(id,host),card=el('section','approval plan-approval');
  card.dataset.request=String(request.id);card.append(el('h3','',t('计划已就绪')));
  const details=el('details'),summary=el('summary','',t('查看计划')),content=el('div','plan-content');
  renderMarkdown(content,request.params.planContent||'',[],()=>null);details.append(summary,content);card.append(details);
  const label=el('label','',t('修改意见（可选）')),input=el('textarea');input.rows=2;input.maxLength=20000;label.append(input);card.append(label);
  const actions=el('div','actions'),yes=el('button','primary',t('按照当前规划结果执行')),revise=el('button','secondary',t('按照修改意见继续规划')),error=el('p','error');
  yes.type=revise.type='button';actions.append(yes,revise);card.append(actions,error);
  let busy=submittedRequestIds.has(request.id);
  function updateActions(){
    const hasFeedback=!!input.value.trim();
    yes.disabled=busy||hasFeedback;revise.disabled=busy||!hasFeedback;
    yes.className=hasFeedback?'secondary':'primary';revise.className=hasFeedback?'primary':'secondary';
  }
  input.oninput=updateActions;updateActions();
  async function submit(action){
    if(busy)return;
    if(action==='implement'&&input.value.trim()){updateActions();return;}
    if(action==='revise'&&!input.value.trim()){error.textContent=t('请填写修改意见');return;}
    const response=action==='implement'?{action}:{action,text:input.value};
    busy=true;approvalBusy++;setLanguageBusy();yes.disabled=revise.disabled=true;error.textContent='';
    try{
      const result=await api(sessionUrl(id,'respond',host),{requestId:request.id,response});
      if(currentId!==id||currentHost!==host)return;
      bumpRecency(id,host);submittedRequestIds.add(request.id);
      if(result.status==='unknown'){error.textContent=t('发送结果尚未确认，请查看会话后再操作。');return;}
      workModes.selection=action==='implement'?'default':'plan';sessionStorage.setItem('work-mode:'+key,workModes.selection);workModes.render();
      card.append(el('p','muted',t('已发送，等待桌面确认…')));
    }catch(e){if(currentId===id&&currentHost===host){error.textContent=t(e.message);busy=false;updateActions();}}
    finally{approvalBusy--;setLanguageBusy();}
  }
  yes.onclick=()=>submit('implement');revise.onclick=()=>submit('revise');return card;
}
async function ignoreSubmission(value){
  const result=await api('/api/accounts/ignore-submission',value);
  const key='pending:'+chatKey(value.threadId,value.host);
  try{if(JSON.parse(sessionStorage.getItem(key))?.id===value.submissionId)sessionStorage.removeItem(key);}catch{}
  if(currentId===value.threadId&&currentHost===value.host&&state){state.submissions=(state.submissions||[]).filter(row=>row.id!==value.submissionId);renderQueue(state.submissions);}
  return result;
}
function renderQueue(rows){
  $('queued').replaceChildren();
  const target=currentId,host=currentHost;
  for(const row of rows){
    const card=el('div','queue-card');
    if(row.status==='waiting_input'&&row.input){
      renderWaitingInput(card,row,target,host);
      $('queued').append(card);
      continue;
    }
    const reply=row.text.startsWith('<send_user_message_question_reply>');
    const failed=row.status==='failed';
    card.append(el('strong','',failed?t('发送失败：')+(row.error||row.lastQueueError||t('Agent 拒绝了这条消息')):row.status==='queued'?t('等待当前任务完成'):t('发送结果待确认')),el('p','',reply?t('问题回复'):row.text));
    if(row.attachments?.length)card.append(el('small','muted',t('附件：')+row.attachments.join('、')));
    if(row.status==='queued'||failed){
      const sendLabel=failed?t('重试发送'):t('立即发送');
      const actions=el('div','queue-actions');
      const sendNow=el('button','plain',sendLabel);
      sendNow.onclick=async()=>{
        sendNow.disabled=true;
        try{
          const result=await api(sessionUrl(target,'queue',host),{id:row.id,action:'send'});
          if(currentId===target&&currentHost===host&&state){
            state.submissions=(state.submissions||[]).filter(item=>item.id!==row.id);
            if(result.status==='unknown')state.submissions.push({...row,status:'unknown'});
            renderQueue(state.submissions);
          }
          toast(result.status==='unknown'?t('发送结果待确认'):failed?t('已重试发送'):t('已立即发送'));
        }catch(e){sendNow.disabled=false;toast(e.message);}
      };
      actions.append(sendNow);
      if(row.editable!==false){
        const edit=el('button','plain',t('修改'));
        edit.onclick=()=>openQueueEditor(row,target,host);
        actions.append(edit);
      }
      const remove=el('button','plain',t('删除'));
      remove.onclick=async()=>{
        remove.disabled=true;
        try{
          await api(sessionUrl(target,'queue',host),{id:row.id,action:'delete'});
          if(currentId===target&&currentHost===host&&state){state.submissions=(state.submissions||[]).filter(item=>item.id!==row.id);renderQueue(state.submissions);}
          toast(t('已删除排队消息'));
        }catch(e){remove.disabled=false;toast(e.message);}
      };
      actions.append(remove);card.append(actions);
    }else{
      card.append(el('p','muted',t('请检查聊天记录。网关不会自动重发这条消息。')));
      const ignore=el('button','plain',t('忽略'));
      ignore.onclick=async()=>{
        if(!window.confirm(t('忽略只会移除未确认提示，不会撤回或重发消息。请先检查聊天记录。')))return;
        ignore.disabled=true;
        try{accountsPanel?.accept(await ignoreSubmission({threadId:target,submissionId:row.id,host}));toast('已忽略未确认记录');}
        catch(e){ignore.disabled=false;toast(e.message);}
      };card.append(ignore);
    }
    $('queued').append(card);
  }
}
// waiting_input 任务卡：复用排队消息区域，展示问题、过期时间与恢复状态。
function renderWaitingInput(card,row,target,host){
  card.append(el('strong','',t('等待补充信息')),el('p','',row.input.summary||row.text));
  const form=el('form'),controls={};
  for(const question of row.input.questions||[]){
    const label=el('label','',question.question+(question.required===false?t('（可选）'):''));
    const input=el('textarea');input.rows=2;input.maxLength=question.maxLength||20000;
    label.append(input);form.append(label);controls[question.id]=input;
  }
  const actions=el('div','queue-actions'),error=el('p','error');
  const submit=el('button','plain',t('提交并恢复任务'));submit.type='submit';
  const remove=el('button','plain',t('取消任务'));remove.type='button';
  remove.onclick=async()=>{
    remove.disabled=true;
    try{
      await api(sessionUrl(target,'queue',host),{id:row.id,action:'delete'});
      if(currentId===target&&currentHost===host&&state){state.submissions=(state.submissions||[]).filter(item=>item.id!==row.id);renderQueue(state.submissions);}
      toast(t('已取消任务'));
    }catch(e){remove.disabled=false;toast(e.message);}
  };
  actions.append(submit,remove);form.append(actions,error);
  form.onsubmit=async event=>{
    event.preventDefault();
    const answers={};
    for(const question of row.input.questions||[])answers[question.id]=controls[question.id].value;
    for(const question of row.input.questions||[])
      if(question.required!==false&&!answers[question.id].trim()){error.textContent=t('请回答所有问题');return;}
    submit.disabled=true;error.textContent='';
    try{
      const result=await api(sessionUrl(target,'input',host),{id:row.id,answers,version:row.input.version});
      toast(result.status==='duplicate'?t('回答已提交过，任务恢复中'):t('已提交，等待恢复'));
      card.append(el('p','muted',t('已提交，等待恢复')));
    }catch(e){error.textContent=t(e.message);submit.disabled=false;}
  };
  card.append(form);
  if(row.input.expiresAt)card.append(el('small','muted',t('过期时间：')+new Date(row.input.expiresAt*1000).toLocaleString(BridgeI18n.locale())));
}
function openQueueEditor(row,target,host){
  let dialog=$('queue-edit-dialog');
  if(!dialog){
    dialog=document.createElement('dialog');dialog.id='queue-edit-dialog';dialog.className='picker';
    dialog.setAttribute('aria-labelledby','queue-edit-heading');
    dialog.innerHTML='<div class="picker-head"><h2 id="queue-edit-heading"></h2><button class="icon-button" type="button" aria-label="关闭"></button></div><div class="picker-body"><form><textarea rows="8" maxlength="100000" aria-label="排队消息"></textarea><p class="error" role="alert"></p><button class="primary" type="submit"></button></form></div>';
    document.body.append(dialog);
    dialog.querySelector('.icon-button').onclick=()=>dialog.close();
    dialog.querySelector('form').onsubmit=async event=>{
      event.preventDefault();
      const targetInfo=dialog.queueTarget;if(!targetInfo)return;
      const text=dialog.querySelector('textarea').value;
      if(!text.trim()){dialog.querySelector('.error').textContent=t('请输入消息');return;}
      const submit=dialog.querySelector('button[type=submit]');submit.disabled=true;dialog.querySelector('.error').textContent='';
      try{
        const result=await api(sessionUrl(targetInfo.thread,'queue',targetInfo.host),{id:targetInfo.id,action:'edit',text});
        if(currentId===targetInfo.thread&&currentHost===targetInfo.host&&state){
          const item=(state.submissions||[]).find(entry=>entry.id===targetInfo.id);
          if(item)item.text=result.text;
          renderQueue(state.submissions||[]);
        }
        dialog.close();toast(t('排队消息已修改'));
      }catch(error){dialog.querySelector('.error').textContent=t(error.message);submit.disabled=false;}
    };
  }
  dialog.queueTarget={id:row.id,thread:target,host};
  dialog.querySelector('h2').textContent=t('修改排队消息');
  dialog.querySelector('.icon-button').setAttribute('aria-label',t('关闭'));
  dialog.querySelector('textarea').setAttribute('aria-label',t('排队消息'));
  dialog.querySelector('textarea').value=row.text;
  dialog.querySelector('button[type=submit]').textContent=t('保存');
  dialog.querySelector('.error').textContent='';dialog.querySelector('button[type=submit]').disabled=false;
  dialog.showModal();dialog.querySelector('textarea').focus();
}
function uuid(){const bytes=crypto.getRandomValues(new Uint8Array(16));bytes[6]=(bytes[6]&15)|64;bytes[8]=(bytes[8]&63)|128;const h=[...bytes].map(x=>x.toString(16).padStart(2,'0')).join('');return h.slice(0,8)+'-'+h.slice(8,12)+'-'+h.slice(12,16)+'-'+h.slice(16,20)+'-'+h.slice(20);}
function syncSendButton(){$('send').disabled=!!state?.readOnly||sending||!state||state.loadingHistory||state.activating||!attachments.ready()||(!$('message').value.trim()&&!attachments.ids().length);}
$('composer').onsubmit=async event=>{event.preventDefault();if(sending||!currentId||state?.readOnly)return;const text=$('message').value;if(!attachments.ready()){toast('请等待附件上传完成');return;}const attachmentIds=attachments.ids(),attachmentPayload=attachmentIds.map(id=>({id}));if(!text.trim()&&!attachmentIds.length)return;const mode=$('send-mode').value,workMode=workModes.value(mode);if(workMode==='goal'&&attachmentIds.length){$('send-error').textContent=t('目标模式暂不支持附件');return;}if(invalidSkills.size){$('send-error').textContent=t('已选 Skill 已失效，请刷新列表后重新选择');return;}const skills=[...selectedSkills].sort();const key='pending:'+chatKey();let pending;try{pending=JSON.parse(sessionStorage.getItem(key));}catch{}const pendingAttachmentIds=(pending?.attachments||[]).map(item=>typeof item==='string'?item:item?.id).filter(Boolean);if(!pending||pending.text!==text||pending.mode!==mode||pending.workMode!==workMode||JSON.stringify(pending.skills||[])!==JSON.stringify(skills)||JSON.stringify(pendingAttachmentIds)!==JSON.stringify(attachmentIds))pending={id:uuid(),text,mode,skills,workMode,attachments:attachmentPayload,uiLocale:BridgeI18n.locale()};sessionStorage.setItem(key,JSON.stringify(pending));sending=true;attachments.setLocked(true);workModes.render(state,mode,true);$('send').disabled=true;$('send-error').textContent='';const target=currentId,targetHost=currentHost;try{const result=await api(sessionUrl(target,'send',targetHost),pending);if(result.status==='ignored')throw Error(t('这条未确认记录已忽略，请刷新后重新编辑消息。'));if(result.status==='unknown')throw Error(t('消息可能已送达，请检查聊天记录。再次点击不会重复提交。'));bumpRecency(target,targetHost);sessionStorage.removeItem(key);workModes.submitted(chatKey(target,targetHost),workMode);attachments.clear(chatKey(target,targetHost),attachmentIds);sessionStorage.removeItem('draft:'+chatKey(target,targetHost));if(currentId===target&&currentHost===targetHost){$('message').value='';selectedSkills.clear();renderSkillPills();window.BridgePresentation?.resizeMessage();}if(result.status==='queued'&&currentId===target&&currentHost===targetHost&&state){state.submissions=[...(state.submissions||[]),{id:pending.id,text,mode:'queue',status:'queued'}];renderQueue(state.submissions);}
toast(workMode==='goal'?(result.activation?.status==='queued'?t('目标已创建，启动已排队'):result.activation?t('目标已创建，正在启动'):t('已提交目标请求，等待桌面确认')):result.status==='queued'?t('已加入待发送队列'):t('已发送到会话'));}catch(error){if(currentId===target&&currentHost===targetHost){const busy=/^(.+) 正在执行。(.+)$/.exec(error.message);$('send-error').textContent=busy?busy[1]+t(' 正在执行。')+t(busy[2]):t(error.message);window.BridgePresentation?.showError();}}finally{sending=false;attachments.setLocked(false);workModes.render(state,$('send-mode').value,false);syncSendButton();}};
$('message').onkeydown=event=>{
  // IME composition Enter (incl. Safari's post-composition keyCode 229) must not send.
  if(event.key!=='Enter'||event.altKey||event.isComposing||event.keyCode===229)return;
  const enterSends=document.documentElement.dataset.enterMode==='send';
  if(event.metaKey||event.ctrlKey||event.shiftKey!==enterSends){event.preventDefault();$('composer').requestSubmit();}
};
$('stop').onclick=async()=>{try{await api('/api/sessions/'+currentId+'/stop',{});toast('已请求停止');}catch(e){toast(e.message);}};
$('history').onclick=async()=>{$('history').disabled=true;try{await api('/api/sessions/'+currentId+'/history',{});}catch(e){toast(e.message);}finally{$('history').disabled=false;}};
$('reconnect').onclick=async()=>{const id=currentId,host=currentHost;$('reconnect').disabled=true;try{await api(sessionUrl(id,'reconnect',host),{activate:true});}catch(e){toast(e.message);}finally{$('reconnect').disabled=false;}};
$('readonly-upgrade').onclick=async()=>{const id=currentId,host=currentHost;if(!id)return;const button=$('readonly-upgrade');button.disabled=true;try{const result=await api(sessionUrl(id,'reconnect',host)+'&upgrade=1',{activate:true});if(result.readOnly){toast(t(result.hint||'桌面端仍在占用此会话'));return;}toast(t('已切换为可写会话；请勿同时在桌面端继续此会话'));if(currentId===id&&currentHost===host)openChat(id,host);}catch(e){toast(e.message);}finally{button.disabled=false;}};
document.querySelectorAll('[data-close]').forEach(button=>button.onclick=()=>$(button.dataset.close).close());
async function loadCatalogKind(kind,refresh=false){
  if(kind!=='models')throw Error(t('未知目录类型'));
  const target=currentId,targetHost=currentHost;
  if(!refresh&&catalogResults[kind])return catalogResults[kind];
  if(!refresh&&catalogTasks[kind])return catalogTasks[kind];
  const request=api('/api/sessions/'+target+'/catalog?kind='+kind+(refresh?'&refresh=true':'')).then(value=>{
    if(currentId!==target||currentHost!==targetHost)throw Error(t('聊天已切换'));
    const payload=value?.[kind]??value?.data??value;
    if(!Array.isArray(payload)){
      const actual=payload===null?'null':typeof payload;
      throw Error(t('模型目录返回无效')+' ('+kind+': '+actual+')');
    }
    catalogData[kind]=payload.map(row=>typeof row==='string'?{id:row,name:row}:row).filter(row=>row&&(row.id||row.model));
    const result={...value,kind,[kind]:catalogData[kind]};
    catalogResults[kind]=result;
    return result;
  }).finally(()=>{if(catalogTasks[kind]&&target===currentId&&targetHost===targetHost)catalogTasks[kind]=null;});
  catalogTasks[kind]=request;
  return request;
}
function resetSkillCatalog(){
  clearTimeout(skillSearchTimer);$('skill-search').value='';$('skill-list').scrollTop=0;skillRequest++;
  catalogData.skills=[];catalogResults.skills=null;catalogTasks.skills=null;
  skillIndex=new Map();skillTotal=0;skillPages=[];skillQuery={q:'',offset:0,limit:200};invalidSkills=new Set();
}
async function loadSkills({refresh=false,append=false}={}){
  const target=currentId,targetHost=currentHost;
  const query={...skillQuery};
  if(!refresh&&!append&&catalogResults.skills)return catalogResults.skills;
  if(!refresh&&!append&&catalogTasks.skills)return catalogTasks.skills;
  if(refresh){
    skillRequest++;catalogTasks.skills=null;catalogResults.skills=null;skillPages=[];skillQuery.offset=0;invalidSkills=new Set();
    $('skill-list').scrollTop=0;
  }
  query.offset=refresh?0:skillQuery.offset;
  const generation=skillRequest;
  const requestKey=chatKey(target,targetHost)+'|'+query.q+'|'+query.offset;
  if(!append&&catalogTasks.skills?.key===requestKey&&catalogTasks.skills?.generation===generation)return catalogTasks.skills;
  const ids=[...selectedSkills].slice(0,8);
  const path='/api/sessions/'+target+'/catalog?kind=skills&q='+encodeURIComponent(query.q)+'&limit='+query.limit+'&offset='+query.offset+(refresh?'&refresh=true':'')+ids.map(id=>'&id='+encodeURIComponent(id)).join('');
  const request=api(path).then(value=>{
    if(currentId!==target||currentHost!==targetHost||generation!==skillRequest)return value;
    const rows=Array.isArray(value?.skills)?value.skills:[];
    if(!Array.isArray(value?.skills))throw Error(t('模型目录返回无效')+' (skills: '+(value?.skills===null?'null':typeof value?.skills)+')');
    if(refresh||!append)skillPages=[];
    const existing=refresh||!append?new Set():new Set(skillPages.map(row=>row.id));
    for(const row of rows){if(!existing.has(row.id)){skillPages.push(row);existing.add(row.id);}}
    catalogData.skills=skillPages;
    for(const row of [...rows,...(Array.isArray(value.selectedSkills)?value.selectedSkills:[])])if(row?.id)skillIndex.set(row.id,row);
    skillTotal=Math.max(0,Number(value.total)||0);
    skillQuery.offset=skillPages.length;
    catalogResults.skills=value;
    const valid=new Set((Array.isArray(value.selectedSkills)?value.selectedSkills:[]).map(row=>row.id));
    const checked=new Set(ids);
    invalidSkills=new Set([...selectedSkills].filter(id=>checked.has(id)?!valid.has(id):invalidSkills.has(id)));
    renderSkillPills();renderSkills();renderSkillCache(value);$('skills-error').textContent=[...(value.errors||[]),value.refreshError].filter(Boolean).join('\n');
    return value;
  }).finally(()=>{
    if(catalogTasks.skills===request&&generation===skillRequest)catalogTasks.skills=null;
  });
  request.key=requestKey;request.generation=generation;catalogTasks.skills=request;
  return request;
}
function renderSkillCache(value){
  const node=$('skills-cache');if(!node)return;
  const state=value?.cache?.state;
  node.textContent=state==='stale'?t('正在显示缓存 Skill，后台会自动刷新。'):state==='legacy'?t('正在显示旧版缓存 Skill，后台会自动刷新。'):'';
}
function startCatalog(){
  loadCatalogKind('models').catch(()=>{});
  resetSkillCatalog();loadSkills().catch(()=>{});
}
function renderEfforts(){const id=$('model-select').value;const models=Array.isArray(catalogData?.models)?catalogData.models:[];const model=models.find(m=>m.id===id);$('custom-model-label').hidden=id!=='__custom__';$('custom-model').required=id==='__custom__';const options=model?.efforts?.length?model.efforts:state?.agent==='codex'?['low','medium','high','xhigh','ultra']:['none','minimal','low','medium','high','xhigh','max','ultra'];$('effort-select').replaceChildren(...options.map(value=>new Option(value,value)));$('effort-select').value=options.includes(state?.effort)?state.effort:model?.defaultEffort||'medium';$('model-description').textContent=model?.description||t('自定义模型需要当前 API 服务支持。');fastModeControl.select(id);$('model-save').disabled=!state?.connected||!id;}
$('model-button').onclick=async()=>{
  const request=++modelRequest,target=chatKey();modelTarget=target;fastModeControl.reset();
  $('model-form').querySelectorAll('input,select,button').forEach(n=>n.disabled=false);
  $('model-error').textContent='';$('model-select').replaceChildren(new Option(t('读取模型列表…'),''));$('model-save').disabled=true;$('model-dialog').showModal();
  try{
    const catalog=await loadCatalogKind('models');
    if(request!==modelRequest||target!==chatKey())return;
    $('model-select').replaceChildren(...(catalog.modelSource==='api'?[new Option(t('请选择上游模型'),'')]:[]),...(Array.isArray(catalog.models)?catalog.models:[]).map(m=>new Option(m.name,m.id)),new Option(t('自定义模型…'),'__custom__'));
    const current=state?.model||catalog.currentModel,models=Array.isArray(catalog.models)?catalog.models:[],known=models.some(m=>m.id===current);$('model-select').value=known?current:catalog.modelSource==='api'&&models.length?'':'__custom__';$('custom-model').value=current||'';
    $('model-error').textContent=catalog.modelError?t(catalog.modelError):catalog.modelSource==='api'&&!known&&models.length?t('当前聊天模型不在上游列表中，请选择可用模型后应用。'):'';
    catalog.kind='models';
    fastModeControl.open(catalog,state,$('model-select').value);renderEfforts();
  }catch(e){if(request===modelRequest&&target===chatKey())$('model-error').textContent=t(e.message);}
};
$('model-select').onchange=renderEfforts;
$('model-form').onsubmit=async event=>{
  event.preventDefault();if(modelSaving||modelTarget!==chatKey()||$('model-save').disabled)return;
  const request=modelRequest,target=currentId,host=currentHost;
  const model=$('model-select').value==='__custom__'?$('custom-model').value.trim():$('model-select').value;
  const body={model,effort:$('effort-select').value,...fastModeControl.payload()};
  modelSaving=true;fastModeControl.setBusy(true);$('model-form').querySelectorAll('input,select,button').forEach(n=>n.disabled=true);$('model-save').disabled=true;$('model-error').textContent='';
  try{
    const result=await api(sessionUrl(target,'settings',host),body);
    if(request===modelRequest&&chatKey(target,host)===chatKey()){$('model-dialog').close();toast(result.confirmed?t('桌面模型设置已同步'):t('桌面已接受设置，等待同步'));}
  }catch(e){if(request===modelRequest&&chatKey(target,host)===chatKey())$('model-error').textContent=t(e.message);}
  finally{modelSaving=false;if(request===modelRequest){$('model-form').querySelectorAll('input,select,button').forEach(n=>n.disabled=false);fastModeControl.setBusy(false);$('model-save').disabled=!state?.connected;}}
};
function renderSkillPills(){
  $('skill-count').textContent=selectedSkills.size?'('+selectedSkills.size+')':'';
  $('skill-pills').replaceChildren();
  for(const id of selectedSkills){
    const skill=skillIndex.get(id),invalid=invalidSkills.has(id);
    const pill=el('button','skill-pill'+(invalid?' invalid':''),(skill?.displayName||(invalid?t('已失效 Skill'):t('已选 Skill')))+' ×');
    pill.type='button';pill.onclick=()=>{selectedSkills.delete(id);invalidSkills.delete(id);renderSkillPills();renderSkills();};
    $('skill-pills').append(pill);
  }
  if(currentId)sessionStorage.setItem('skills:'+chatKey(),JSON.stringify([...selectedSkills]));
}
function renderSkills(){
  const list=$('skill-list'),rows=Array.isArray(catalogData?.skills)?catalogData.skills:[];
  const search=$('skill-search').value.toLocaleLowerCase();
  const visible=search===skillQuery.q.toLocaleLowerCase()?rows:rows.filter(skill=>[skill.name,skill.displayName,skill.description].join(' ').toLocaleLowerCase().includes(search));
  list.replaceChildren();
  for(const skill of visible){
    const label=el('label','skill-option');
    const input=el('input');input.type='checkbox';input.value=skill.id;input.checked=selectedSkills.has(skill.id);
    input.onchange=()=>{
      if(input.checked&&selectedSkills.size>=8){input.checked=false;toast('最多选择 8 个 Skill');return;}
      if(input.checked){selectedSkills.add(skill.id);invalidSkills.delete(skill.id);}else selectedSkills.delete(skill.id);
      renderSkillPills();
    };
    const content=el('span');content.append(el('strong','',skill.displayName),el('small','',skill.description));
    label.append(input,content);list.append(label);
  }
  if(!visible.length)list.append(el('p','muted',t('没有匹配的 Skill')));
}
async function openSkills(refresh=false){
  if(!currentId)return;$('skills-error').textContent='';if(!$('skills-dialog').open)$('skills-dialog').showModal();
  if(catalogData.skills?.length)renderSkills();
  else $('skill-list').textContent=t('正在读取已安装 Skill…');
  try{await loadSkills({refresh});renderSkills();renderSkillPills();}
  catch(e){
    if(catalogData.skills?.length)renderSkills();
    else $('skill-list').textContent=t('Skill 加载失败，请点击刷新重试');
    $('skills-error').textContent=t(e.message);renderSkillPills();
  }
}
function skillScroll(){
  const list=$('skill-list');
  if(skillTotal>skillPages.length&&list.scrollHeight-list.clientHeight-list.scrollTop<240&&!catalogTasks.skills)loadSkills({append:true}).catch(e=>toast(e.message));
}
$('skills-button').onclick=()=>openSkills();$('skills-refresh').onclick=()=>openSkills(true);
$('skill-list').onscroll=skillScroll;
$('skill-search').oninput=()=>{clearTimeout(skillSearchTimer);skillSearchTimer=setTimeout(()=>{skillRequest++;skillQuery.q=$('skill-search').value;skillQuery.offset=0;skillPages=[];catalogData.skills=[];catalogResults.skills=null;catalogTasks.skills=null;$('skill-list').scrollTop=0;renderSkills();loadSkills().catch(e=>{$('skills-error').textContent=t(e.message);});},200);};
window.addEventListener('pagehide',saveDraft);
function startFromLink(token){return start(token).catch(error=>{showLogin();$('login-error').textContent=t(error.message);});}
startFromLink(initialPairingToken);initialPairingToken=null;
window.addEventListener('hashchange',()=>{const token=takePairingToken();if(token!==null)startFromLink(token);});
function setLanguageBusy(){$('phone-language').disabled=!!(approvalBusy||creatingChat);$('appearance-language').disabled=$('phone-language').disabled;}
$('phone-language').value=BridgeI18n.language();
$('phone-language').onchange=()=>{
  if(approvalBusy||creatingChat)return;
  const replies=[...$('approvals').querySelectorAll('input,textarea,select')].map(node=>({value:node.value,checked:node.checked}));
  BridgeI18n.setLanguage($('phone-language').value);BridgeI18n.apply();accountPanel.render();if(typeof accountsPanel!=="undefined")accountsPanel?.render();renderList();
  if(!$('new-chat-title').dataset.edited)$('new-chat-title').value=t('新聊天');
  renderLoginStatus();
  for(const id of ['login-error','send-error','model-error','skills-error','new-chat-error','notify-error','rename-error','toast'])$(id).textContent=t($(id).textContent);
  renderNewChatProjects();
  updateNewChatDesc();
  if($('model-select').querySelector('[value=__custom__]'))$('model-select').querySelector('[value=__custom__]').textContent=t('自定义模型…');
  if(state){approvalStamp='';renderState(state);[...$('approvals').querySelectorAll('input,textarea,select')].forEach((node,i)=>{if(replies[i]){node.value=replies[i].value;node.checked=replies[i].checked;if(node.tagName==='SELECT')node.dispatchEvent(new Event('change'));}});}
  else syncDocumentTitle();
  $('notify-button').textContent=t(notificationState.watching?'提醒已开':'提醒');
  if(chatTimeline)chatTimeline.relabel();messageActions.relabel();
  window.BridgePresentation?.relabel();attachments.render();renderActivity();
  renderSkillPills();if(catalogData)renderSkills();
};
BridgeI18n.apply();

let renameTarget=null,renameBusy=false;
$('rename-chat').onclick=()=>{
  if(!currentId||renameBusy)return;
  renameTarget={id:currentId,host:currentHost};
  $('rename-title').value=state?.title||$('chat-title').textContent;
  $('rename-error').textContent='';$('chat-details-dialog').close();$('rename-dialog').showModal();
};
$('rename-form').onsubmit=async event=>{
  event.preventDefault();if(renameBusy||!renameTarget)return;
  const target=renameTarget,title=$('rename-title').value.trim();
  if(!title)return;
  renameBusy=true;$('rename-save').disabled=true;$('rename-error').textContent='';
  try{
    const result=await api(sessionUrl(target.id,'rename',target.host),{title});
    if(currentId===target.id&&currentHost===target.host){
      if(state)state.title=result.title;
      syncDocumentTitle();
      $('chat-title').textContent=result.title;$('details-title').textContent=result.title;
    }
    $('rename-dialog').close();toast('聊天名称已修改');
    await loadList(true);
  }catch(error){$('rename-error').textContent=t(error.message);}
  finally{renameBusy=false;$('rename-save').disabled=false;}
};

let deleteChatBusy=false;
$('delete-chat').onclick=async()=>{
  if(!currentId||deleteChatBusy)return;
  const target={id:currentId,host:currentHost};
  const native=target.host==='local'&&(state?.agent==='codex'||!state);
  const confirmText=native?t('将从电脑 Codex 中永久删除这条聊天及其记录，无法撤销。确定删除？'):t('将从列表移除这条会话；Agent 侧的记录不会被删除。确定移除？');
  if(!window.confirm(confirmText))return;
  deleteChatBusy=true;$('delete-chat').disabled=true;
  try{
    const result=await api(sessionUrl(target.id,'delete',target.host),{});
    $('chat-details-dialog').close();
    toast(result.mode==='deleted'?t('会话已删除'):t('已从列表移除会话'));
    listRevision=null;
    if(currentId===target.id&&currentHost===target.host){$('back').onclick();}
    else await loadList(true);
  }catch(error){toast(error.message);}
  finally{deleteChatBusy=false;$('delete-chat').disabled=false;}
};

const accountsPanel=typeof AccountsPanel==='undefined'?null:new AccountsPanel({root:$('accounts-content'),read:()=>api('/api/accounts'),request:({action,...value})=>action==='ignoreSubmission'?ignoreSubmission(value):api(action==='details'?'/api/accounts/details':'/api/accounts/switch',value),onChanged:()=>accountPanel.clear(),onUpdate:value=>{accountPanel.schedule();const official=value.current?.kind==='chatgpt';$('account-button').hidden=!official;if(!official)$('account-details').open=false;const label=value.current?.kind==='api'?'API 接入':official?'账号与额度':'账号与接入';$('accounts-button').setAttribute('data-i18n',label);$('accounts-button').textContent=t(label);}});
const poolPanel=typeof PoolPanel==='undefined'?null:new PoolPanel({root:$('pool-content'),read:()=>api('/api/pool/kimi'),request:(action,payload)=>api('/api/pool/kimi/'+action,payload||{})});
if(accountsPanel)$('accounts-button').onclick=()=>{$('accounts-dialog').showModal();accountsPanel.refresh();if(poolPanel)poolPanel.refresh();};
