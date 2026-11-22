'use strict';
const timelineText=source=>typeof BridgeI18n==='undefined'?source:BridgeI18n.t(source);

// 工具调用行不止一种 kind：ACP 管线的 toolCall、桌面回放的 storedToolEvent、
// MCP 的 mcpToolCall/dynamicToolCall。折叠行为按「是否工具行」统一判定。
const TOOL_ROW_KINDS=/^(toolCall|tool_call|storedToolEvent|mcpToolCall|dynamicToolCall)$/;
// userIndex contains only user rows and omits role, so recognize its protocol entries by prefix too.
const internalTeamProtocolRow=row=>(row?.role==='user'||row?.role==null)&&/^\s*\[(?:Team delegation|Team context|Team review|Team historical issue routing|Team issue routing|Team account quotas|Team background tasks)\]/i.test(String(row.text||''));
// Kimi 把后台任务事件作为 origin=task 的回合注入，正文是 <notification …> 包裹的纯文本。
// 按系统通知展示：不当作用户发言，不进入跳转列表，也不提供编辑/分支。
// 截断预览可能缺少闭合标签，因此只要求开头是通知且闭合标签之后没有其他正文。
const taskNotice=(row,text=row?.text)=>{
  if(row?.role!=='user'&&row?.role!=null)return null;
  const match=String(text||'').match(/^\s*<notification\b([^>]*)>([\s\S]*?)(?:<\/notification>\s*)?$/);
  if(!match||match[2].includes('</notification>'))return null;
  const attrs={};for(const [,key,value] of match[1].matchAll(/([\w:-]+)="([^"]*)"/g))attrs[key]=value;
  let title='',severity=attrs.severity||'';const lines=[];
  for(const line of match[2].trim().split('\n')){
    const field=line.match(/^\s*(Title|Severity)\s*:\s*(.*?)\s*$/i);
    if(field&&/^title$/i.test(field[1])&&!title){title=field[2];continue;}
    if(field&&!severity)severity=field[2];
    lines.push(line);
  }
  return {title:title||attrs.type||'',severity:severity.toLowerCase().replace(/[^a-z]/g,''),text:lines.join('\n').trim()};
};
const commandRow=row=>row.role==='activity'&&(/^(commandExecution|CommandExecution)$/.test(row.kind)||row.toolKind==='execute');
const sleepRow=row=>row.role==='activity'&&row.kind==='sleep';
const toolRow=row=>row.role==='activity'&&(TOOL_ROW_KINDS.test(row.kind)||commandRow(row)||sleepRow(row));
const steeredRow=row=>row.role==='activity'&&(row.kind==='steered'||row.title==='steered');
const maintenanceRow=row=>row.role==='activity'&&(row.kind==='gatewayMigration'||row.kind==='gatewayCompaction');
const reasoningRow=row=>row.role==='activity'&&(row.kind==='reasoning'||row.kind==='agentThought');
const approvalReviewRow=row=>row.role==='activity'&&(row.kind==='automaticApprovalReview'||row.title==='automaticApprovalReview');
const approvalReviewStatus=status=>timelineText({approved:'已批准',allow:'已批准',denied:'已拒绝',rejected:'已拒绝',deny:'已拒绝',inProgress:'审查中',pending:'审查中',completed:'审查已结束',failed:'审查失败',interrupted:'已中断',cancelled:'已取消'}[status]||'状态待同步');
const approvedReview=(row,text)=>{
  if(row.status&&row.status!=='completed')return /^(approved|allow)$/.test(row.status);
  try{const value=JSON.parse(text);return /^(approved|allow)$/.test(value?.decision||value?.review?.decision||value?.status);}catch{return false;}
};
const compactionRow=row=>row.role==='activity'&&(row.kind==='contextCompaction'||row.title==='contextCompaction');
const compactionStatusText=status=>timelineText({inProgress:'压缩中',completed:'已完成',interrupted:'已中断'}[status]||'状态待同步');
const compactionContent=(row,text)=>{
  let status=row.status,source,value;
  try{value=JSON.parse(text);}catch{}
  if(value&&typeof value==='object'&&!Array.isArray(value)&&value.type==='contextCompaction'){
    status=row.turnStatus==='completed'||value.completed!==false?'completed':!row.turnStatus||row.turnStatus==='inProgress'?'inProgress':'interrupted';
    source=value.source??'automatic';
  }else if(!/^\s*[\[{]/.test(String(text||''))){
    return {status,text};
  }else if(!status&&row.turnStatus==='completed')status='completed';
  const lines=[timelineText('压缩状态')+'：'+compactionStatusText(status)];
  if(source==='manual'||source==='automatic')lines.push(timelineText('触发方式')+'：'+timelineText(source==='manual'?'手动':'自动'));
  return {status,text:lines.join('\n')};
};
const sleepContent=(row,text)=>{
  let duration=row.durationMs,value;
  if(duration==null){try{value=JSON.parse(text);}catch{}if(value?.type==='sleep')duration=value.durationMs;}
  if(typeof duration!=='number'||!Number.isFinite(duration)||duration<0)return timelineText('等待时长未知');
  if(duration<1000)return timelineText('等待')+' '+duration+timelineText(' 毫秒');
  return timelineText('等待')+' '+Number((duration/1000).toFixed(3))+timelineText(' 秒');
};
// 兼容旧网关把未知事件序列化为 JSON 的历史投影；只生成文本节点。
const approvalReviewText=(text,row)=>{
  let value;try{value=JSON.parse(text);}catch{
    if(/^\s*[\[{]/.test(String(text||'')))return timelineText('审批状态')+'：'+approvalReviewStatus(row.status)+'\n'+timelineText('展开查看审查依据');
    return text;
  }
  if(!value||typeof value!=='object'||Array.isArray(value))return text;
  const labels={status:'审批状态',decision:'审批决定',reason:'原因',rationale:'审查依据',summary:'摘要',riskLevel:'风险等级',risk:'风险',command:'命令',review:'审查结果',assessment:'评估',action:'操作',userAuthorization:'用户授权',cwd:'工作目录'};
  const lines=[];
  const visit=(object,depth=0)=>{
    for(const key of Object.keys(labels)){
      const item=object[key];
      if(item==null||!Object.prototype.hasOwnProperty.call(labels,key))continue;
      const label=timelineText(labels[key]||key),prefix='  '.repeat(depth);
      if(typeof item==='object'){lines.push(prefix+label+'：');visit(item,depth+1);}
      else{
        const level={low:'低',medium:'中',high:'高',unknown:'未知'};
        const value=key==='status'||key==='decision'?approvalReviewStatus(item):['riskLevel','risk','userAuthorization'].includes(key)&&level[item]?timelineText(level[item]):String(item);
        lines.push(prefix+label+'：'+value);
      }
    }
  };
  visit(value);return lines.join('\n')||timelineText('审批审查已记录');
};
const commandState=row=>{
  const status=String(row.status||'');
  if(/^(cancelled|canceled|interrupted|stopped)$/.test(status))return'cancelled';
  if(/^(failed|error)$/.test(status))return'failed';
  if(/^(outcome_unknown|unknown)$/.test(status))return'unknown';
  if(typeof row.exitCode==='number')return row.exitCode===0?'completed':'failed';
  if(/^(completed|complete|success|succeeded)$/.test(status))return row.toolKind==='execute'?'completed':'unknown';
  return'inProgress';
};
const commandStateText=state=>timelineText({inProgress:'进行中',completed:'成功',failed:'失败',cancelled:'已取消',unknown:'结果未知'}[state]||'结果未知');

// Agent 送来的工具标题形态各异：纯工具名（"Bash"）、「工具名: 命令」、
// 「工具名(命令)」，甚至整条命令自带状态后缀（CodeBuddy 的 "git add … · completed"）。
// 折叠态只要一个简短可辨的标签：有干净的工具名前缀就取前缀，否则截取命令首行。
const toolLabel=title=>{
  let label=String(title||'').replace(/\s*[·•‧・]\s*(completed|failed|error|inProgress|running|pending|进行中|已完成|失败)\s*$/i,'').trim();
  // 前缀须像工具名（无空格引号括号）才剥离；否则 ":" 多半在命令内部，
  // 例如 git commit -m "fix(list): xxx"，剥了会把命令截成碎片。
  const prefix=label.match(/^([^\s"'`（(]{1,24})\s*[:：]\s/)||label.match(/^([A-Za-z][\w.-]{0,23})\s*[（(]/);
  if(prefix)return prefix[1];
  const chars=[...label.split('\n')[0].trim()];
  if(chars.length>60)return chars.slice(0,60).join('')+'…';
  return chars.join('')||timelineText('工具调用');
};

// Heading label for one row; plan rows surface live completion counts.
const rowLabel=(row,text=row.text)=>{
  const notice=taskNotice(row,text);
  if(notice)return timelineText('后台任务通知')+(notice.title?' · '+notice.title:'');
  if(row.role!=='activity')return row.role==='user'?timelineText('你'):timelineText('执行错误');
  if(maintenanceRow(row))return row.text;
  if(steeredRow(row))return timelineText('已向进行中的对话补充指示');
  if(approvalReviewRow(row))return timelineText('自动审批审查')+(row.status?' · '+approvalReviewStatus(row.status):'');
  if(compactionRow(row))return timelineText('上下文压缩')+' · '+compactionStatusText(compactionContent(row,text).status);
  if(sleepRow(row))return timelineText('等待');
  if(reasoningRow(row))return timelineText('思考摘要');
  if(commandRow(row)){
    const state=commandState(row);
    return timelineText('执行命令')+(state==='completed'||state==='failed'?'':'（'+commandStateText(state)+'）');
  }
  let label=row.title||row.kind;
  if(toolRow(row))label=toolLabel(row.title);
  if(row.kind==='plan'&&row.entries?.length){
    const done=row.entries.filter(entry=>entry.status==='completed').length;
    label+=' · '+done+'/'+row.entries.length+(done===row.entries.length?' ✓':'');
  }else if(row.status==='inProgress')label+=timelineText(' · 进行中');
  if(row.kind==='subagent'){
    const meta=[row.subagentId,row.subagentType,row.status].filter(Boolean).join(' · ');
    if(meta)label+=' · '+meta;
  }
  return label;
};

// History and live changes share stable keys, but never share a request queue.
class ChatTimeline {
  constructor({url, request, renderMeta, renderText, status, onAction, hideInternalTeamMessages=false,initialTurnId=null}) {
    Object.assign(this,{url,request,renderMeta,renderText,status,onAction,hideInternalTeamMessages});
    if(typeof currentTeam!=='undefined'&&currentTeam)this.hideInternalTeamMessages=true;
    this.viewportLocked=!!(typeof currentTeam!=='undefined'&&currentTeam&&typeof teamCanvas!=='undefined'&&teamCanvas);
    this.viewport=document.getElementById('timeline');
    this.container=document.getElementById('messages');
    this.older=document.getElementById('older');
    this.newer=document.getElementById('newer');
    this.userJump=document.getElementById('user-jump');
    if(typeof window!=='undefined')this.userJump.open=window.matchMedia('(min-width:721px)').matches;
    this.userJumpList=document.getElementById('user-jump-list');
    this.userJumpCount=document.getElementById('user-jump-count');
    this.userJumpButtons=new Map();
    this.userIndex=null;
    this.initialTurnId=initialTurnId;
    this.abort=new AbortController();this.rows=new Map();this.nodes=new Map();this.details=new Map();
    this.files=new Map();this.sequence=-1;this.epoch='';this.before=null;this.hasMore=false;
    this.busy=false;this.generation=0;this.windowRevision=0;this.following=true;this.lastTop=0;this.retryCount=0;
    this.showReasoning=document.documentElement.dataset.showReasoning!=='false';
    this.showProcess=document.documentElement.dataset.showProcess!=='false';
    this.onScroll=()=>{
      if(this.suppress)return;
      const upward=this.viewport.scrollTop<this.lastTop;
      this.lastTop=this.viewport.scrollTop;this.following=this.atBottom();this.anchor=this.capture();
      this.syncNewer();
      this.updateUserJumpActive();
      if(upward&&this.nearTop()&&!this.retryCount)this.loadOlder(100);
    };
    this.viewport.addEventListener('scroll',this.onScroll,{passive:true});
    this.older.onclick=()=>this.sequence<0?this.start():this.loadOlder(this.retryCount||100);
    this.newer.onclick=()=>{this.jumpBottom();this.anchor=this.capture();};
    this.resize=new ResizeObserver(()=>this.restore(this.anchor,this.following));
    this.resize.observe(this.container);this.syncNewer();
  }
  dispose(){this.subscription?.();this.subscription=null;this.abort.abort();clearTimeout(this.startRetry);this.loadErrorEl?.remove();this.resize.disconnect();this.viewport.removeEventListener('scroll',this.onScroll);this.newer.hidden=true;this.userJump.hidden=true;}
  atBottom(){return this.viewport.scrollHeight-this.viewport.scrollTop-this.viewport.clientHeight<110;}
  nearTop(){const nodes=[...this.container.children].filter(n=>!n.hidden);return nodes.length>0&&nodes.slice(0,11).some(n=>n.getBoundingClientRect().bottom>=this.viewport.getBoundingClientRect().top);}
  capture(accept=()=>true){const top=this.viewport.getBoundingClientRect().top;const nodes=[...this.container.children].filter(n=>!n.hidden&&!n.dataset.summary&&accept(n));const node=nodes.find(n=>n.getBoundingClientRect().bottom>top)||nodes[nodes.length-1];return node?{key:node.dataset.key,offset:node.getBoundingClientRect().top-top}:null;}
  setViewportLocked(locked){this.viewportLocked=!!locked;if(this.viewportLocked){this.viewport.scrollTop=0;this.lastTop=0;this.anchor=null;this.following=true;}}
  rowVisible(row){
    if(this.hideInternalTeamMessages&&internalTeamProtocolRow(row))return false;
    if(row.role==='user'||row.role==='error'||maintenanceRow(row))return true;
    if(reasoningRow(row))return this.showReasoning;
    if(row.role==='activity'||row.phase==='commentary')return this.showProcess;
    return true;
  }
  setVisibility({showReasoning,showProcess}){
    if(this.showReasoning===showReasoning&&this.showProcess===showProcess)return;
    this.showReasoning=showReasoning;this.showProcess=showProcess;
    const anchor=this.capture(node=>this.rowVisible(this.rows.get(node.dataset.key).row)),bottom=this.following;
    for(const [key,{node}] of this.nodes)node.hidden=!this.rowVisible(this.rows.get(key).row);
    this.restore(anchor,bottom);this.fillRecent();
  }
  syncNewer(){this.newer.hidden=this.following;if(this.following)this.newer.textContent=timelineText('跳转到最后 ↓');}
  jumpBottom(){this.viewport.scrollTop=this.viewport.scrollHeight;this.following=true;this.syncNewer();}
  restore(anchor,bottom=false){
    if(this.abort.signal.aborted)return;
    if(this.viewportLocked){this.viewport.scrollTop=0;this.lastTop=0;this.anchor=null;this.following=true;return;}
    this.suppress=true;
    const node=anchor&&this.nodes.get(anchor.key)?.node;
    if(bottom)this.jumpBottom();
    else if(node&&!node.hidden)this.viewport.scrollTop+=node.getBoundingClientRect().top-this.viewport.getBoundingClientRect().top-anchor.offset;
    this.lastTop=this.viewport.scrollTop;
    this.anchor=this.capture();
    requestAnimationFrame(()=>this.suppress=false);
  }
  // The panel lists every prompt, not just the loaded window: the server
  // ships a complete userIndex in timeline meta; rows loaded afterwards
  // (fresh prompts, newly paged history) overlay it by key.
  renderUserJump(ordered,bottom=false){
    const merged=new Map();
    if(this.userIndex)for(const entry of this.userIndex)merged.set(entry.key,entry);
    for(const row of ordered)if(row.role==='user')merged.set(row.key,row);
    const rows=[...merged.values()].filter(row=>!taskNotice(row)&&(!this.hideInternalTeamMessages||!internalTeamProtocolRow(row)));
    this.userJump.hidden=!rows.length;this.userJumpCount.textContent=rows.length?String(rows.length):'';
    this.userJumpList.replaceChildren();this.userJumpButtons.clear();
    for(const [index,row] of rows.entries()){
      const label=(row.text||'').trim().replace(/\s+/g,' ')||timelineText('用户消息');
      const button=document.createElement('button');button.type='button';button.className='user-jump-item';button.dataset.key=row.key;button.title=label;
      const number=document.createElement('span');number.className='user-jump-index';number.textContent=String(index+1);
      const text=document.createElement('span');text.className='user-jump-text';text.textContent=label;
      button.append(number,text);button.onclick=()=>this.jumpToUser(row.key);
      this.userJumpButtons.set(row.key,button);this.userJumpList.append(button);
    }
    this.updateUserJumpActive(bottom);
  }
  async jumpToUser(key){
    const pages=!this.nodes.get(key)?await this.loadUntilKey(key):0;
    const target=this.nodes.get(key)?.node;
    // 跳到旧消息可能要翻最多 24 轮历史：给一句结果反馈，找不到也要说明，而不是静默无反应。
    if(!target){globalThis.BridgeFeedback?.ok('未找到该条消息，已加载可用历史');return;}
    if(pages)globalThis.BridgeFeedback?.ok('已加载历史消息并跳转');
    this.suppress=true;
    this.viewport.scrollTop+=target.getBoundingClientRect().top-this.viewport.getBoundingClientRect().top-12;
    this.lastTop=this.viewport.scrollTop;this.following=this.atBottom();this.anchor=this.capture();
    this.syncNewer();
    this.updateUserJumpActive(false,key);
    requestAnimationFrame(()=>this.suppress=false);
  }
  async jumpToTurn(turnId){
    for(let pages=0;pages<=24&&!this.abort.signal.aborted;pages++){
      const ordered=[...this.rows.values()].map(entry=>entry.row).sort((a,b)=>a.order-b.order);
      const rows=ordered.filter(row=>row.turnId===turnId&&this.rowVisible(row));
      const target=rows.find(row=>row.role==='user')||rows[0];
      // A page may begin halfway through this turn. Load its beginning before
      // choosing the prompt, otherwise a later steer or tool row wins.
      if(target&&(!this.hasMore||ordered[0]?.turnId!==turnId)){await this.jumpToUser(target.key);return true;}
      if(!this.hasMore||pages===24)break;
      while(this.busy&&!this.abort.signal.aborted)await this.pause(40);
      if(this.abort.signal.aborted)return false;
      await this.loadOlder(100);
    }
    if(!this.abort.signal.aborted)globalThis.BridgeFeedback?.ok('未找到对应任务回合，已打开原会话');
    return false;
  }
  // Jumping to a prompt older than the loaded window pages history up until
  // the row exists, bounded so a huge chat cannot page forever.
  async loadUntilKey(key){
    let pages=0;
    for(let i=0;i<24&&this.hasMore&&!this.nodes.has(key)&&!this.abort.signal.aborted;i++){
      while(this.busy&&!this.abort.signal.aborted)await this.pause(40);
      if(!this.hasMore||this.nodes.has(key))break;
      await this.loadOlder(100);pages++;
    }
    return pages;
  }
  updateUserJumpActive(bottom=false,selected=''){
    const keys=[...this.userJumpButtons.keys()];if(!keys.length)return;
    let active=selected||(bottom?keys[keys.length-1]:keys[0]);
    if(!selected&&!bottom){
      const top=this.viewport.getBoundingClientRect().top+72;
      for(const key of keys){
        const node=this.nodes.get(key)?.node;
        if(node&&node.getBoundingClientRect().top<=top)active=key;
        else if(node)break;
      }
    }
    for(const [key,button] of this.userJumpButtons){
      button.classList.toggle('active',key===active);
      if(key===active)button.setAttribute('aria-current','location');else button.removeAttribute('aria-current');
    }
    if(bottom)this.userJumpList.scrollTop=this.userJumpList.scrollHeight;
  }
  read(action,params={}){return this.request(this.url(action)+'&'+new URLSearchParams(params),undefined,this.abort.signal);}
  // 重试按钮下的服务端错误详情；空文本时靠 .error:empty 自动隐藏。
  loadError(message){
    if(!this.loadErrorEl){
      this.loadErrorEl=document.createElement('p');
      this.loadErrorEl.className='error load-error';
      this.older.insertAdjacentElement('afterend',this.loadErrorEl);
    }
    this.loadErrorEl.textContent=message||'';
  }
  async start(){
    if(this.starting||this.abort.signal.aborted)return;
    clearTimeout(this.startRetry);this.starting=true;this.loadError('');
    this.older.hidden=false;this.older.disabled=true;this.older.textContent=timelineText('正在读取最近内容…');
    try{
      const page=await this.read('timeline',{limit:20});
      if(this.abort.signal.aborted)return;
      this.apply(page,true);this.updates();
      // Give the first 20 records a paint before quietly filling the 100-row window.
      requestAnimationFrame(()=>requestAnimationFrame(()=>this.fillRecent()));
    }catch(e){if(this.abort.signal.aborted)return;this.older.disabled=false;this.older.textContent=timelineText('读取失败，点击重试');this.status(e.message);this.loadError(e.message);this.startRetry=setTimeout(()=>this.start(),3000);}
    finally{this.starting=false;}
  }
  fillRecent(){
    const filtered=!this.showReasoning||!this.showProcess,target=filtered?20:100;
    const count=filtered?[...this.nodes.values()].filter(({node})=>!node.hidden).length:this.rows.size;
    if(!this.abort.signal.aborted&&this.rows.size>0&&count<target&&!this.retryCount)this.loadOlder(filtered?100:target-count);
  }
  // A network restore or foreground return retries a failed recent-window
  // fill; the retry counter otherwise stays armed until the user taps
  // "load older", which is invisible on a phone after a socket blip.
  refill(){this.retryCount=0;this.historyStatus();this.fillRecent();}
  historyStatus(){this.older.hidden=!this.hasMore;this.older.disabled=this.busy;this.older.textContent=this.busy?timelineText('正在读取更早内容…'):this.retryCount?timelineText('历史加载失败，点击重试'):timelineText('查看更早内容');}
  async loadOlder(count){
    if(this.busy||!this.hasMore||this.abort.signal.aborted)return;
    const generation=this.generation,before=this.before;
    this.busy=true;this.retryCount=0;this.historyStatus();
    try{
      const page=await this.read('timeline',{before,limit:count});
      if(this.abort.signal.aborted||generation!==this.generation)return;
      if(page.reset||page.epoch!==this.epoch){this.apply(page,true);return;}
      this.apply(page,false,true);
    }catch(e){if(this.abort.signal.aborted||generation!==this.generation)return;this.retryCount=count;}
    finally{this.busy=false;if(!this.abort.signal.aborted){this.historyStatus();this.fillRecent();}}
  }
  apply(page,reset=false,history=false){
    if(!history&&page.epoch===this.epoch&&page.sequence<this.sequence)return;
    const anchor=this.capture(),bottom=this.following;
    if(reset){this.generation++;this.rows.clear();this.nodes.clear();this.details.clear();this.files.clear();this.container.replaceChildren();this.epoch=page.epoch;this.retryCount=0;}
    for(const file of page.files||[])this.files.set(file.id,file);
    if((!history&&page.sequence>=this.sequence)||reset){this.sequence=page.sequence;this.meta=page.meta;this.renderMeta({...page.meta,files:[...this.files.values()]});}
    if(page.meta?.userIndex)this.userIndex=page.meta.userIndex;
    if(!history)this.pendingRequests=!!page.meta.requests?.length;
    let changed=false;
    for(const row of page.rows){
      const previous=this.rows.get(row.key);
      if(previous&&previous.sequence>page.sequence)continue;
      this.rows.set(row.key,{row,sequence:page.sequence});
      const stamp=JSON.stringify(row),old=this.nodes.get(row.key);
      if(old?.stamp===stamp)continue;
      const open=old?.node.tagName==='DETAILS'&&old.node.open;
      const node=this.renderRow(row,open);node.dataset.key=row.key;
      node.hidden=!this.rowVisible(row);changed=changed||!node.hidden;
      if(old)old.node.replaceWith(node);
      this.nodes.set(row.key,{node,stamp});
    }
    const ordered=[...this.rows.values()].map(x=>x.row).sort((a,b)=>a.order-b.order);
    let position=this.container.firstChild;
    for(const row of ordered){const node=this.nodes.get(row.key).node;if(node===position)position=position.nextSibling;else this.container.insertBefore(node,position);}
    if(reset||history){this.before=page.before;this.hasMore=page.hasMore;this.windowRevision++;}
    this.renderTurnSummaries(ordered);
    this.refreshActions();this.historyStatus();this.restore(anchor,bottom);
    this.renderUserJump([...this.rows.values()].map(x=>x.row).sort((a,b)=>a.order-b.order),bottom);
    if(!history&&!bottom&&(changed||page.meta.requests?.length)){this.newer.textContent=page.meta.requests?.length?timelineText('有待确认请求 ↓'):timelineText('有新内容 ↓');this.newer.hidden=false;}
    if(reset)requestAnimationFrame(()=>this.fillRecent());
    if(this.initialTurnId&&this.rows.size&&!page.meta?.loadingHistory){
      const turnId=this.initialTurnId;this.initialTurnId=null;
      requestAnimationFrame(()=>this.jumpToTurn(turnId));
    }
  }
  // Pure-append delta frames: patch the streamed rows' text in place instead
  // of re-reading whole rows. The server only sends a delta when every changed
  // row appends onto the exact version the phone holds; any other shape (and
  // any row the phone does not hold at prevVersion) falls back to one changes
  // re-read, so the merge stays keyed+version safe.
  applyDelta(frame){
    if(frame.epoch!==this.epoch||frame.sequence<=this.sequence)return;
    const cursor={after:this.sequence,epoch:this.epoch,start:this.before||''};
    if((frame.rows||[]).some(delta=>{const entry=this.rows.get(delta.key);return !entry||entry.sequence>=frame.sequence||entry.row.version!==delta.prevVersion;}))return this.recoverDelta(cursor);
    const anchor=this.capture(),bottom=this.following;
    this.sequence=frame.sequence;
    for(const file of frame.files||[])this.files.set(file.id,file);
    if(frame.meta){this.meta=frame.meta;this.renderMeta({...frame.meta,files:[...this.files.values()]});if(frame.meta.userIndex)this.userIndex=frame.meta.userIndex;}
    let changed=false;
    for(const delta of frame.rows||[]){
      const entry=this.rows.get(delta.key);
      entry.row.text+=delta.append;entry.row.version=delta.version;
      if(entry.sequence<frame.sequence)entry.sequence=frame.sequence;
      const cached=this.details.get(delta.key);
      if(cached){
        if(cached.version===delta.prevVersion){
          cached.version=delta.version;
          if(cached.next===null)cached.text+=delta.append;
        }else this.details.delete(delta.key);
      }
      const stamp=JSON.stringify(entry.row),old=this.nodes.get(delta.key);
      if(old?.stamp===stamp)continue;
      const open=old?.node.tagName==='DETAILS'&&old.node.open;
      const node=this.renderRow(entry.row,open);node.dataset.key=delta.key;
      node.hidden=!this.rowVisible(entry.row);changed=changed||!node.hidden;
      if(old)old.node.replaceWith(node);
      this.nodes.set(delta.key,{node,stamp});
    }
    this.refreshActions();this.historyStatus();this.restore(anchor,bottom);
    this.renderUserJump([...this.rows.values()].map(x=>x.row).sort((a,b)=>a.order-b.order),bottom);
    if(!bottom&&changed){this.newer.textContent=this.pendingRequests?timelineText('有待确认请求 ↓'):timelineText('有新内容 ↓');this.newer.hidden=false;}
  }
  recoverDelta(cursor){
    if(this.deltaRecovery)return this.deltaRecovery;
    const generation=this.generation;
    this.deltaRecovery=this.read('changes',cursor).then(page=>{
      if(this.abort.signal.aborted||generation!==this.generation)return;
      this.apply(page,!!page.reset||page.epoch!==this.epoch);
    }).catch(()=>{
      if(this.abort.signal.aborted||generation!==this.generation)return;
      this.status(timelineText('重新连接中'));
      this.subscription?.();this.subscription=null;
      clearTimeout(this.startRetry);
      this.startRetry=setTimeout(()=>{if(!this.abort.signal.aborted&&generation===this.generation)this.updates();},1000);
    }).finally(()=>{this.deltaRecovery=null;});
    return this.deltaRecovery;
  }
  renderRow(row,open){
    const notice=taskNotice(row),activity=row.role==='activity'||!!notice,
          steered=steeredRow(row),maintenance=maintenanceRow(row),
          planEntries=activity&&row.kind==='plan'&&row.entries?.length?row.entries:null,
          node=document.createElement(activity&&!steered&&!maintenance?'details':'div');
    node.className=activity?'activity timeline-row'+((steered||maintenance)?' status-event':''):'message timeline-row '+row.role;
    if(activity&&row.kind)node.dataset.kind=row.kind;
    if(notice){node.dataset.kind='taskNotification';if(notice.severity)node.dataset.severity=notice.severity;}
    // CSS 以 data-tool（而非具体 kind）区分工具行：所有管线的工具调用折叠态一致。
    if(toolRow(row))node.dataset.tool='';
    if(commandRow(row)){node.dataset.command='';node.dataset.commandStatus=commandState(row);}
    const heading=document.createElement(activity&&!steered?'summary':'span');heading.className=activity?'':'who';
    // 助手消息不再逐条标注 Agent：一条会话只属于一个 Agent，身份在标题栏展示。
    if(activity||row.role==='user'||row.role==='error'){
      heading.textContent=rowLabel(row);
      // 命令按结构化结果显示图标；失败即使缺退出码也保留。
      const commandResult=commandRow(row)?commandState(row):'',commandFailed=commandResult==='failed';
      if(activity&&(typeof row.exitCode==='number'||commandFailed||commandResult==='completed')){
        const failed=commandFailed||(typeof row.exitCode==='number'&&row.exitCode!==0);
        const exit=document.createElement('span');
        exit.className='diff-exit '+(failed?'fail':'ok');
        exit.textContent=failed?'✗':'✓';
        exit.setAttribute('aria-hidden','true');
        heading.append(' ',exit);
      }
      node.append(heading);
    }
    if(steered||maintenance)return node;
    const reasoning=reasoningRow(row);
    const body=document.createElement(activity&&!planEntries&&!reasoning?'pre':'div');
    body.className=activity?(planEntries?'activity-body plan-body':reasoning?'activity-body message-body reasoning-body':'activity-body'):'message-body';
    const more=document.createElement('button');more.className='plain detail-more';more.type='button';
    const cached=this.details.get(row.key);
    if(cached&&cached.version!==row.version)this.details.delete(row.key);
    const paint=()=>{
      const detail=this.details.get(row.key),text=detail?.text??row.text;
      body.replaceChildren();
      if(planEntries){
        // The structured checklist replaces the plain-text preview; its
        // entries are always complete, so there is no detail to fetch.
        const list=document.createElement('ul');list.className='plan-entries';
        for(const entry of planEntries){
          const item=document.createElement('li');
          item.className='plan-entry status-'+String(entry.status||'pending').replace(/[^a-z_]/g,'');
          const mark=document.createElement('span');mark.className='plan-mark';
          mark.textContent=entry.status==='completed'?'✓':entry.status==='in_progress'?'▸':'○';
          const content=document.createElement('span');content.className='plan-text';
          content.textContent=entry.content||'';
          item.append(mark,content);list.append(item);
        }
        body.append(list);body.hidden=false;more.hidden=true;
      }else{
        if(notice){
          heading.textContent=rowLabel(row,text);
          body.textContent=taskNotice(row,text)?.text??text;
        }else if(sleepRow(row)){
          body.textContent=sleepContent(row,text);
        }else if(approvalReviewRow(row)){
          node.dataset.approvalStatus=approvedReview(row,text)?'approved':'';
          body.textContent=approvalReviewText(text,row);
        }else if(compactionRow(row)){
          const content=compactionContent(row,text);
          heading.textContent=timelineText('上下文压缩')+' · '+compactionStatusText(content.status);
          body.textContent=content.text;
        }else if(activity&&!reasoning)body.textContent=text;else this.renderText(body,text,[...this.files.values()],()=>this.fullText(row));
        if(activity)body.hidden=!text;
        more.hidden=detail?detail.next===null:!row.truncated;
        more.textContent=detail?timelineText('继续加载正文'):timelineText('展开完整内容');
      }
    };
    const load=async()=>{
      if(more.disabled||this.abort.signal.aborted)return;
      const prior=this.details.get(row.key);if(prior?.next===null)return;
      more.disabled=true;more.textContent=timelineText('正在读取…');
      try{
        const detail=await this.read('detail',{key:this.epoch+'.'+row.key,offset:prior?.next||0,version:prior?.version||row.version});
        if(this.abort.signal.aborted||!node.isConnected||this.rows.get(row.key)?.row.version!==detail.version)return;
        for(const file of detail.files||[])this.files.set(file.id,file);
        this.details.set(row.key,{version:detail.version,text:(detail.offset?prior?.text||'':'')+detail.text,next:detail.next});paint();
      }catch(e){if(!this.abort.signal.aborted){more.hidden=false;more.textContent=timelineText('读取失败，点击重试');}}
      finally{more.disabled=false;}
    };
    more.onclick=load;node.append(body,more);paint();
    if(!notice&&(row.role==='user'||row.role==='assistant')){
      const actions=document.createElement('div');actions.className='message-actions';
      if((row.text||'').trim()){
        const copy=document.createElement('button');copy.type='button';copy.className='plain message-copy';copy.textContent=timelineText('复制');copy.onclick=()=>window.BridgeClipboard?.copy(()=>this.fullText(row),copy);actions.append(copy);
      }
      node.append(actions);
    }
    if(row.attachments?.length){
      const attachments=document.createElement('div');attachments.className='timeline-attachments';
      for(const item of row.attachments){
        if((item.type==='localImage'||item.type==='image')&&(item.uploadId||item.desktopId)){
          const source=BridgeConnection.url(item.uploadId
            ?this.url('uploads/'+encodeURIComponent(item.uploadId)+'/preview')
            :this.url('desktop-images/'+encodeURIComponent(item.desktopId)));
          const original=item.uploadId
            ?source+'&variant=original'
            :source;
          // The anchor keeps target=_blank for long-press/new-tab; a plain
          // tap is intercepted by the image lightbox via data-image-preview.
          const link=document.createElement('a');link.href=original;link.target='_blank';link.rel='noopener';
          link.dataset.imagePreview=original;link.dataset.imageName=item.name||timelineText('图片附件');
          const image=document.createElement('img');image.className='attachment-image';image.alt=item.name||timelineText('图片附件');
          image.loading='lazy';image.decoding='async';image.src=source;
          image.onerror=()=>{if(!image.dataset.publicFallback)link.remove();};link.append(image);attachments.append(link);
        }
      }
      const attachment=document.createElement('small');attachment.className='muted';
      attachment.textContent=timelineText('附件：')+row.attachments.map(a=>a.name||a.type).join('、');
      attachments.append(attachment);node.append(attachments);
    }
    if(activity){node.open=!!open;node.ontoggle=()=>{if(node.open&&!this.details.has(row.key)&&!planEntries)load();};}
    return node;
  }
  // 每轮「变更文件」汇总卡:按 turnId 聚合各行的 changes(同路径去重、
  // 取最新),放在该轮最后一行之后。节点不在 this.nodes 里,仅由本方法
  // 重建;capture() 会跳过它们以保持滚动锚定在真实行上。
  renderTurnSummaries(ordered){
    const expanded=new Set([...this.container.querySelectorAll('[data-summary][open]')].map(node=>node.dataset.turnId));
    for(const node of this.container.querySelectorAll('[data-summary]'))node.remove();
    const cwd=this.meta?.cwd||'',byTurn=new Map();
    for(const row of ordered){
      if(!row.changes?.length)continue;
      let map=byTurn.get(row.turnId);
      if(!map)byTurn.set(row.turnId,map=new Map());
      for(const change of row.changes)map.set(change.path,change);
    }
    for(const [turnId,map] of byTurn){
      const last=ordered.filter(row=>row.turnId===turnId).pop();
      const anchor=last&&this.nodes.get(last.key)?.node;
      if(!anchor)continue;
      const card=window.BridgeDiff.summaryCard([...map.values()],cwd,change=>window.BridgeDiff.open(change,cwd));
      card.dataset.summary='1';
      card.dataset.turnId=String(turnId);
      card.open=expanded.has(String(turnId));
      anchor.after(card);
    }
  }
  async fullText(row){
    const valid=()=>!this.abort.signal.aborted&&this.rows.get(row.key)?.row.version===row.version;
    if(!valid())throw Error(timelineText('消息内容已变化，请重新打开编辑'));
    if(!row.truncated)return row.text;
    let text='',offset=0;
    do{
      const detail=await this.read('detail',{key:this.epoch+'.'+row.key,offset,version:row.version});
      if(!valid()||detail.version!==row.version||detail.offset!==offset||detail.key!==row.key)throw Error(timelineText('消息内容已变化，请重新打开编辑'));
      text+=detail.text;
      if(detail.next===null)return text;
      if(detail.next<=offset)throw Error(timelineText('读取完整正文失败，请重试'));
      offset=detail.next;
    }while(true);
  }
  refreshActions(){
    for(const [key,{node}] of this.nodes){
      const row=this.rows.get(key)?.row,bar=node.querySelector('.message-actions');if(!bar||!row)continue;
      if(!this.onAction)continue;
      const add=(action,label,disabled)=>{const cls=action==='fork'?'message-fork':'message-edit';let b=bar.querySelector('.'+cls);if(!b){b=document.createElement('button');b.type='button';b.className='plain '+cls;bar.append(b);}b.textContent=timelineText(label);b.disabled=disabled;b.onclick=()=>this.onAction(action,row,this);};
      if(row.editable){
        const latest=this.meta?.latestUserTurnId===row.turnId;
        // ACP 的 message-action 只支持 fork/edit-fork；原地 edit 仅有 Codex 原生实现。
        // 最新用户消息在 ACP 会话里也必须走分支，不能先显示可提交再靠后端报错。
        const inPlace=latest&&this.meta?.agent==='codex';
        add(inPlace?'edit':'edit-fork',inPlace?'编辑并重新发送':'编辑并新建分支',row.turnStatus==='inProgress'||latest&&this.meta?.status==='active');
      }
      if(row.forkable)add('fork','从这里分支',false);
    }
  }
  relabel(){
    this.refreshActions();
    this.container.querySelectorAll('.message-copy').forEach(n=>n.textContent=timelineText('复制'));
    this.container.querySelectorAll('.code-copy').forEach(n=>n.textContent=timelineText('复制代码'));
    this.historyStatus();
    for(const [key,entry] of this.nodes){
      const row=this.rows.get(key)?.row;if(!row)continue;
      const node=entry.node,heading=node.querySelector(':scope>.who,:scope>summary');
      if(heading)heading.textContent=rowLabel(row,this.details.get(key)?.text??row.text);
      const more=node.querySelector('.detail-more');
      if(more&&!more.disabled)more.textContent=timelineText(this.details.has(key)?'继续加载正文':'展开完整内容');
      const attachment=node.querySelector('.timeline-attachments small.muted');
      if(attachment)attachment.textContent=timelineText('附件：')+row.attachments.map(a=>a.name||a.type).join('、');
    }
    const ordered=[...this.rows.values()].map(x=>x.row).sort((a,b)=>a.order-b.order);
    this.renderTurnSummaries(ordered);this.renderUserJump(ordered,this.following);
    this.newer.textContent=timelineText(this.pendingRequests?'有待确认请求 ↓':'有新内容 ↓');
  }
  updates(){
    this.subscription?.();
    // Live frames arrive via the shared transport (SSE when the gateway
    // advertises it, changes long-poll on tunnels). The cursor is re-read
    // per request so history expansion never strands a delta, and rows
    // merge by key+version, so no in-flight generation guard is needed.
    // Delta frames only ever carry appends onto versions this client holds;
    // applyDelta re-reads changes itself when one does not line up.
    this.subscription=BridgeTransport.subscribe(this.url,{
      onChanges:page=>{if(this.abort.signal.aborted)return;this.apply(page,false);this.fillRecent();},
      onDelta:frame=>{if(this.abort.signal.aborted)return;this.applyDelta(frame);},
      onReset:page=>{if(this.abort.signal.aborted)return;this.apply(page,true);this.fillRecent();},
      onSynchronized:()=>{if(!this.abort.signal.aborted)this.syncState='live';},
      onStatus:text=>{if(!this.abort.signal.aborted)this.status(text==='reconnecting'?timelineText('重新连接中'):text);},
      cursor:()=>({after:this.sequence,epoch:this.epoch,start:this.before||''})
    });
  }
  pause(ms){return new Promise(resolve=>{const done=()=>{clearTimeout(timer);this.abort.signal.removeEventListener('abort',done);resolve();};const timer=setTimeout(done,ms);this.abort.signal.addEventListener('abort',done,{once:true});});}
}
if(typeof module!=='undefined')module.exports={ChatTimeline};
