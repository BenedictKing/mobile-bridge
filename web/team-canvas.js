'use strict';

function teamCanvasAssociations(inputs,issues){
  const inputByID=new Map((inputs||[]).map(input=>[input.inputId||input.id,input]));
  const associations={};
  for(const issue of issues||[]){
    for(const source of issue.inputs||[]){
      const input=inputByID.get(source.inputId);
      if(!input||Number(input.revision||1)!==Number(source.revision||1))continue;
      const linked=associations[source.inputId]||[];
      if(!linked.includes(issue.id))linked.push(issue.id);
      associations[source.inputId]=linked;
    }
  }
  return associations;
}

function teamCanvasLinkedRecords(bubbleRecords,issueRecords){
  const byInputID=new Map(bubbleRecords.map(record=>[record.input.inputId||record.input.id,record]));
  return issueRecords.map(record=>({...record,bubbles:record.bubbles
    .map(bubble=>byInputID.get(bubble.input.inputId||bubble.input.id)).filter(Boolean)}));
}

function teamCanvasInputRunID(input,issueIDs){
  if(issueIDs?.length)return'';
  const runIDs=Array.isArray(input?.runIds)?input.runIds:[];
  return runIDs[runIDs.length-1]||'';
}

function teamCanvasFitTransform(viewportWidth,viewportHeight,canvasWidth,canvasHeight,maxScale=1){
  if(viewportWidth<=0||viewportHeight<=0||canvasWidth<=0||canvasHeight<=0)return null;
  const scale=Math.min(maxScale,(viewportWidth-48)/canvasWidth,(viewportHeight-48)/canvasHeight);
  return{scale,x:(viewportWidth-canvasWidth*scale)/2,y:(viewportHeight-canvasHeight*scale)/2};
}

function teamCanvasOwnerLine(item,t=value=>value,agentLabel=value=>value){
  const displayName=owner=>{
    let kind=owner?.agentKind||'';
    if(!kind&&owner?.name===owner?.memberId&&String(owner.memberId).startsWith('member-'))kind=owner.memberId.slice('member-'.length);
    if(kind){const label=agentLabel(kind);if(label&&label!==kind)return label;}
    return owner?.name;
  };
  if(item?.currentOwner?.name)return t('当前执行人')+' · '+displayName(item.currentOwner);
  if(item?.lastOwner?.name)return t('最后执行人')+' · '+displayName(item.lastOwner);
  return'';
}

function teamCanvasDeliveryRows(run,t=value=>value){
  const delivery=run?.delivery||{},result=delivery.resultSummary||{},rows=[];
  if(result.text)rows.push(result.text+(result.truncated?' · '+t('内容已截断'):''));
  if(delivery.review)rows.push(t('评审结果')+' · '+t(delivery.review.verdict||'状态待同步')+(delivery.review.stale?' · '+t('评审结果已过期'):''));
  if(delivery.verify){
    const label=delivery.verify.configured===false?t('未配置'):t(delivery.verify.status||'待验证');
    rows.push(t('验证结果')+' · '+label+(delivery.verify.exitCode!==undefined?' · '+t('退出码')+' '+delivery.verify.exitCode:''));
  }
  if(Number(delivery.pendingQuestions)>0)rows.push(t('待回答问题')+' · '+delivery.pendingQuestions);
  for(const workspace of delivery.workspaces||[])rows.push(t('工作区')+' · '+(workspace.workspaceId||workspace.taskId||''));
  return rows;
}

class TeamCanvasController{
  constructor({root,request,openRun,notify}){
    Object.assign(this,{root,request,openRun,notify});
    this.current=null;this.inputs=[];this.issues=[];
    this.positions=new Map();this.mode='canvas';this.generation=0;this.pollTimer=null;this.refreshToken=null;
    this.transform={x:0,y:0,scale:1};this.drag=null;this.detailIssue=null;this.detailRun=null;this.detailRequestToken=null;
    this.viewport=document.getElementById('team-canvas-viewport');
    this.conversationBar=document.getElementById('team-conversation-view-bar');
    this.world=document.getElementById('team-canvas-world');
    this.nodes=document.getElementById('team-canvas-nodes');
    this.wires=document.getElementById('team-canvas-wires');
    this.detail=document.getElementById('team-canvas-detail');
    this.summary=document.getElementById('team-canvas-summary');
    this.error=document.getElementById('team-canvas-error');
    this.zoomLabel=document.getElementById('team-canvas-zoom');
    this.bindControls();
  }
  t(value){return typeof BridgeI18n!=='undefined'?BridgeI18n.t(value):value;}
  node(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;}
  button(text,className,action){const button=this.node('button',text,className);button.type='button';if(action)button.addEventListener('click',action);return button;}
  bindControls(){
    document.querySelectorAll('[data-team-view]').forEach(button=>button.addEventListener('click',()=>this.setMode(button.dataset.teamView)));
    document.getElementById('team-open-canvas').addEventListener('click',()=>this.setMode('canvas'));
    document.getElementById('team-canvas-refresh').addEventListener('click',()=>this.refresh(true));
    document.getElementById('team-canvas-backfill').addEventListener('click',()=>this.backfillHistory());
    document.getElementById('team-canvas-fit').addEventListener('click',()=>this.fitAll());
    document.getElementById('team-canvas-zoom-out').addEventListener('click',()=>this.zoomAt(this.transform.scale/1.2));
    document.getElementById('team-canvas-zoom-in').addEventListener('click',()=>this.zoomAt(this.transform.scale*1.2));
    document.getElementById('team-canvas-detail-close').addEventListener('click',()=>this.closeDetail());
    document.getElementById('team-canvas-open-run').addEventListener('click',()=>{
      const target=this.detailRun;if(target&&this.openRun)this.openRun(this.current.id,target.id);
    });
    this.viewport.addEventListener('pointerdown',event=>this.startPan(event));
    this.viewport.addEventListener('pointermove',event=>this.movePointer(event));
    this.viewport.addEventListener('pointerup',event=>this.endPointer(event));
    this.viewport.addEventListener('pointercancel',event=>this.endPointer(event));
    this.viewport.addEventListener('wheel',event=>{
      if(event.target.closest('.team-canvas-detail'))return;
      event.preventDefault();
      if(event.ctrlKey||event.metaKey)this.zoomAt(this.transform.scale*(event.deltaY<0?1.08:.92),event.clientX,event.clientY);
      else{this.transform.x-=event.deltaX;this.transform.y-=event.deltaY;this.applyTransform();}
    },{passive:false});
    window.addEventListener('resize',()=>{if(this.root.hidden)return;this.render();});
  }
  open(team){
    clearInterval(this.pollTimer);
    this.generation++;this.current=team||null;this.inputs=[];this.issues=[];this.historyBackfill=null;this.backfillBusy=false;this.positions.clear();this.userInteracted=false;this.transform={x:0,y:0,scale:1};this.refreshToken=null;this.refreshQueued=false;this.closeDetail();
    this.nodes.replaceChildren();this.wires.replaceChildren();this.summary.textContent='';
    if(!team){this.root.hidden=true;this.conversationBar.hidden=true;document.getElementById('chat').classList.remove('team-canvas-active');return;}
    this.mode=this.savedMode(team.id);this.root.hidden=this.mode!=='canvas';
    document.getElementById('chat').classList.toggle('team-canvas-active',this.mode==='canvas');
    if(this.mode==='canvas'&&this.viewport.closest('.timeline'))this.viewport.closest('.timeline').scrollTop=0;
    this.syncModeButtons();
    this.refresh();
    this.pollTimer=setInterval(()=>this.refresh(),5000);
  }
  close(){this.open(null);}
  accepted(input){
    if(input?.inputId&&this.current?.id===input.teamId){
      const existing=this.inputs.findIndex(row=>(row.inputId||row.id)===input.inputId);
      const row={...input,associationStatus:input.associationStatus||'pending',queueStatus:input.queueStatus||'queued'};
      if(existing>=0)this.inputs.splice(existing,1,row);else this.inputs.push(row);
      this.render();
      requestAnimationFrame(()=>this.focusMessage(row.inputId));
    }
    this.refresh(true);
  }
  async backfillHistory(){
    if(!this.current||this.backfillBusy)return;
    const button=document.getElementById('team-canvas-backfill');
    const team=this.current;this.backfillBusy=true;button.disabled=true;
    try{
      const result=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/issues/backfill',{
        method:'POST',body:{},
      });
      if(this.current?.id!==team.id)return;
      this.historyBackfill={active:Number(result.pending||0)>0,pending:Number(result.pending||0),started:Number(result.started||0),failed:Number(result.failed||0)};
      this.notify?.(result.pending
        ?result.pending+' '+this.t('条历史消息待整理')+' · '+this.t('整理期间可以继续发送新要求')
        :result.failed?this.t('历史对话已检查')+' · '+result.failed+' '+this.t('条需要处理'):this.t('历史整理完成'));
      await this.refresh(true);
    }catch(error){
      if(this.current?.id===team.id)this.error.textContent=this.t(error.message||'整理历史对话失败');
    }finally{this.backfillBusy=false;button.disabled=false;}
  }
  savedMode(teamID){try{return localStorage.getItem('team-chat-view:'+teamID)==='conversation'?'conversation':'canvas';}catch{return'canvas';}}
  setMode(mode){
    if(!this.current||!['canvas','conversation'].includes(mode))return;
    this.mode=mode;try{localStorage.setItem('team-chat-view:'+this.current.id,mode);}catch{}
    if(typeof chatTimeline!=='undefined')chatTimeline?.setViewportLocked(mode==='canvas');
    this.root.hidden=mode!=='canvas';
    document.getElementById('chat').classList.toggle('team-canvas-active',mode==='canvas');
    this.syncModeButtons();
    if(mode==='canvas'){
      this.userInteracted=false;
      if(this.viewport.closest('.timeline'))this.viewport.closest('.timeline').scrollTop=0;
      requestAnimationFrame(()=>{if(!this.userInteracted)this.fitAll(false,true);});
      this.refresh();
    }
  }
  afterHistoryRead(){
    if(!this.current||this.mode!=='canvas')return;
    if(typeof chatTimeline!=='undefined')chatTimeline?.setViewportLocked(true);
    if(this.viewport.closest('.timeline'))this.viewport.closest('.timeline').scrollTop=0;
    requestAnimationFrame(()=>{if(!this.userInteracted)this.fitAll(false,true);});
  }
  syncModeButtons(){
    document.querySelectorAll('[data-team-view]').forEach(button=>button.setAttribute('aria-pressed',String(button.dataset.teamView===this.mode)));
    this.conversationBar.hidden=!this.current||this.mode!=='conversation';
  }
  async refresh(force=false){
    const team=this.current;if(!team)return;
    if(this.refreshToken){if(force)this.refreshQueued=true;return;}
    const token={};this.refreshToken=token;const generation=this.generation;this.error.textContent='';
    try{
      const [issueResult,inputResult]=await Promise.allSettled([
        this.request('/api/teams/'+encodeURIComponent(team.id)+'/issues'),
        this.loadInputs(team.id,generation),
      ]);
      if(generation!==this.generation||this.current?.id!==team.id)return;
      if(issueResult.status==='fulfilled')this.issues=Array.isArray(issueResult.value.issues)?issueResult.value.issues:[];
      else this.error.textContent=this.t(issueResult.reason?.message||'读取 Issue 失败');
      if(inputResult.status==='fulfilled')this.inputs=inputResult.value;
      else this.error.textContent=this.t(inputResult.reason?.message||'读取输入失败');
      if(generation===this.generation&&this.current?.id===team.id){this.render();this.checkHistoryBackfill();}
    }catch(error){if(generation===this.generation)this.error.textContent=this.t(error.message||'画布刷新失败');}
    finally{if(this.refreshToken===token)this.refreshToken=null;if(this.refreshQueued){this.refreshQueued=false;queueMicrotask(()=>this.refresh(true));}}
  }
  async loadInputs(teamID,generation){
    const rows=[];let cursor='';
    do{
      const suffix=cursor?'&cursor='+encodeURIComponent(cursor):'';
      const page=await this.request('/api/teams/'+encodeURIComponent(teamID)+'/inputs?limit=100'+suffix);
      if(generation!==this.generation)return rows;
      if(Array.isArray(page.inputs))rows.push(...page.inputs);
      cursor=page.nextCursor||'';
    }while(cursor);
    return rows;
  }
  statusLabel(issue){
    const labels=typeof TEAM_RUN_STATUS_LABELS!=='undefined'?TEAM_RUN_STATUS_LABELS:{};
    const statusLabels={linking:'等待执行路由',clarification:'需要澄清'};
    return this.t(statusLabels[issue.status]||labels[issue.status]||issue.status||'状态待同步');
  }
  progress(issue){
    const counts=issue?.taskCounts;if(!counts||typeof counts!=='object')return null;
    let total=0,completed=0;
    for(const [status,value] of Object.entries(counts)){const count=Number(value)||0;total+=count;if(status==='completed')completed=count;}
    return total?{total,completed,percent:Math.round(completed/total*100)}:null;
  }
  buildGroups(){
    const associations=teamCanvasAssociations(this.inputs,this.issues);
    const inputs=[...this.inputs].sort((a,b)=>Number(a.sequence||0)-Number(b.sequence||0));
    const bubbles=inputs.map((input,index)=>({input,index:index+1,issueIDs:associations[input.inputId||input.id]||[]}));
    const inputByID=new Map(bubbles.map(row=>[row.input.inputId||row.input.id,row]));
    const issues=this.issues.map(issue=>({issue,bubbles:(issue.inputs||[]).map(source=>inputByID.get(source.inputId)).filter(Boolean)}));
    return{bubbles,issues};
  }
  checkHistoryBackfill(){
    if(!this.historyBackfill?.active)return;
    const historical=this.inputs.filter(input=>input.historical),pending=historical.filter(input=>input.associationStatus==='pending').length;
    this.historyBackfill.pending=pending;
    if(pending===0){
      this.historyBackfill.active=false;
      const failed=historical.filter(input=>input.associationStatus==='failed').length;
      const clarification=historical.filter(input=>input.associationStatus==='clarification').length;
      this.notify?.(this.t('历史整理完成')+' · '+(historical.length-failed)+' '+this.t('条已处理')+' · '+clarification+' '+this.t('条需澄清')+' · '+failed+' '+this.t('条失败'));
    }
  }
  render(){
    if(this.root.hidden||this.mode!=='canvas')return;
    this.nodes.replaceChildren();this.wires.replaceChildren();
    const {bubbles,issues}=this.buildGroups();
    const pending=bubbles.filter(({input})=>input.associationStatus==='pending').length;
    const summary=[bubbles.length+' '+this.t('条气泡'),issues.length+' '+this.t('个事项'),pending+' '+this.t('待关联')];
    if(this.historyBackfill?.active)summary.push(this.t('正在整理历史')+' · '+this.historyBackfill.pending);
    this.summary.textContent=summary.join(' · ');
    const bubbleRecords=bubbles.map(record=>{
      const node=this.renderMessage(record);node.style.width='320px';this.nodes.append(node);return{...record,node};
    });
    const issueRecords=issues.map(record=>{
      const node=this.renderIssue(record.issue);node.style.width='360px';this.nodes.append(node);
      return{...record,node};
    });
    this.lastRecords={bubbles:bubbleRecords,issues:teamCanvasLinkedRecords(bubbleRecords,issueRecords)};this.layout(bubbleRecords,issueRecords);
    this.renderWires(this.lastRecords);
    if(!this.userInteracted)requestAnimationFrame(()=>{if(!this.userInteracted)this.fitAll(false,true);});
  }
  associationLabel(input,issueIDs){
    const runHint=teamCanvasInputRunID(input,issueIDs)?' · '+this.t('查看 Run'):'';
    if(input.associationStatus==='clarification')return this.t('需要澄清')+(issueIDs.length?' · '+issueIDs.length+' '+this.t('个事项'):runHint);
    if(issueIDs.length)return this.t('已关联')+' · '+issueIDs.length+' '+this.t('个事项');
    if(input.historical&&input.associationStatus==='pending')return this.t('历史整理中');
    const labels={pending:'正在关联',clarification:'需要澄清',ignored:'无需建项',failed:'关联失败',unclassified:'历史输入未归类'};
    return this.t(labels[input.associationStatus]||'关系待同步')+runHint;
  }
  renderMessage(record){
    const {input,index,issueIDs}=record,id=input.inputId||input.id;
    const linkedRunID=teamCanvasInputRunID(input,issueIDs);
    const button=this.button('', 'team-canvas-node team-canvas-message');button.dataset.inputId=id;
    const head=this.node('div',undefined,'team-canvas-message-head');
    head.append(this.node('span',String(input.sequence||index).padStart(2,'0'),'team-canvas-message-index'));
    if(input.acceptedAt){
      const at=new Date(input.acceptedAt);
      if(Number.isFinite(at.getTime()))head.append(this.node('time',at.toLocaleTimeString(BridgeI18n.locale(),{hour:'2-digit',minute:'2-digit'}),'team-canvas-message-time'));
    }
    const relation=this.associationLabel(input,issueIDs);
    head.append(this.node('span',relation,'team-canvas-message-relation'));
    button.title=input.text||this.t('附件消息');
    button.append(head,this.node('p',input.text||this.t('附件消息'),'team-canvas-message-text'));
    if(input.associationReason)button.append(this.node('span',input.associationReason,'team-canvas-message-relation'));
    button.setAttribute('aria-label','#'+index+' · '+(input.text||this.t('附件消息'))+' · '+relation);
    button.addEventListener('click',()=>{
      if(Date.now()<(this.suppressClickUntil||0))return;
      if(issueIDs.length){for(const issueID of issueIDs)this.focusItem(issueID);}
      else if(linkedRunID&&this.openRun)this.openRun(this.current.id,linkedRunID);
    });
    this.bindNodeDrag(button,()=>({key:'m:'+id,record}));
    return button;
  }
  renderIssue(issue){
    const button=this.button('', 'team-canvas-node team-canvas-issue');button.dataset.issueId=issue.id;
    const head=this.node('div',undefined,'team-canvas-issue-head');
    head.append(this.node('span',this.t('事项')+' · '+this.shortID(issue.id),'team-canvas-issue-id'));
    head.append(this.node('span',this.statusLabel(issue),'team-canvas-status'));button.append(head);
    button.append(this.node('strong',issue.title||this.t('团队事项'),'team-canvas-issue-title'));
    const progress=this.progress(issue);
    if(progress){
      const progressBox=this.node('div',undefined,'team-canvas-progress');
      const progressHead=this.node('div',undefined,'team-canvas-progress-head');
      progressHead.append(this.node('strong',progress.completed+'/'+progress.total+' '+this.t('项已完成')));
      progressHead.append(this.node('span',progress.percent+'%'));
      const track=this.node('div',undefined,'team-canvas-progress-track'),fill=this.node('span',undefined,'team-canvas-progress-fill');fill.style.width=progress.percent+'%';track.append(fill);progressBox.append(progressHead,track);button.append(progressBox);
    }else button.append(this.node('p',this.t('进度待同步'),'team-canvas-issue-muted'));
    const bits=[];
    const owner=teamCanvasOwnerLine(issue,value=>this.t(value),kind=>typeof BridgeI18n!=='undefined'?BridgeI18n.agentLabel(kind):kind);if(owner)bits.push(owner);
    if(issue.lastUpdate?.summary)bits.push(this.t('最近回音')+' · '+issue.lastUpdate.summary);
    if(issue.nextAction?.label)bits.push(this.t('下一步')+' · '+issue.nextAction.label);
    const meta=this.node('div',undefined,'team-canvas-issue-meta');
    for(const bit of bits)meta.append(this.node('span',bit));
    button.append(meta,this.node('span',this.t('查看事项详情'),'team-canvas-open-hint'));
    button.setAttribute('aria-label',this.shortID(issue.id)+' '+(issue.title||'')+' · '+this.statusLabel(issue)+' · '+this.t('查看事项详情'));
    button.addEventListener('click',()=>{if(Date.now()>=(this.suppressClickUntil||0))this.openDetail(issue,button);});
    this.bindNodeDrag(button,()=>({key:'i:'+issue.id,issue}));
    return button;
  }
  layout(bubbleRecords,issueRecords){
    const mobile=window.matchMedia('(max-width:720px)').matches;
    if(this.mobileLayout!==undefined&&this.mobileLayout!==mobile)this.positions.clear();
    let width=mobile?380:940,height=0;
    const setPosition=(node,key,x,y)=>{
      const override=this.positions.get(key);if(override){x=override.x;y=override.y;}
      node.style.left=x+'px';node.style.top=y+'px';node._canvasPosition={x,y,key};
      const box={x,y,width:node.offsetWidth,height:node.offsetHeight};return box;
    };
    if(mobile){
      let y=28;
      for(const record of bubbleRecords){
        const box=setPosition(record.node,'m:'+record.input.inputId,18,y);y+=box.height+12;
      }
      y+=18;
      for(const record of issueRecords){const box=setPosition(record.node,'i:'+record.issue.id,18,y);y+=box.height+24;}
      height=Math.max(600,y+30);
    }else{
      let bubbleY=28,issueY=28;
      for(const record of bubbleRecords){const box=setPosition(record.node,'m:'+record.input.inputId,24,bubbleY);bubbleY+=box.height+14;}
      for(const record of issueRecords){const box=setPosition(record.node,'i:'+record.issue.id,460,issueY);issueY+=box.height+22;}
      height=Math.max(620,bubbleY,issueY)+24;
    }
    this.world.style.width=width+'px';this.world.style.height=height+'px';
    this.wires.setAttribute('viewBox',`0 0 ${width} ${height}`);this.wires.setAttribute('width',width);this.wires.setAttribute('height',height);
    this.canvasWidth=width;this.canvasHeight=height;this.mobileLayout=mobile;
  }
  renderWires(records){
    this.wires.replaceChildren();
    const defs=document.createElementNS('http://www.w3.org/2000/svg','defs'),marker=document.createElementNS('http://www.w3.org/2000/svg','marker');
    marker.setAttribute('id','team-canvas-arrow');marker.setAttribute('viewBox','0 0 10 10');marker.setAttribute('refX','9');marker.setAttribute('refY','5');marker.setAttribute('markerWidth','6');marker.setAttribute('markerHeight','6');marker.setAttribute('orient','auto-start-reverse');
    const arrow=document.createElementNS('http://www.w3.org/2000/svg','path');arrow.setAttribute('d','M 0 0 L 10 5 L 0 10 z');arrow.setAttribute('class','team-canvas-arrow');marker.append(arrow);defs.append(marker);this.wires.append(defs);
    for(const record of records.issues||[]){
      for(const bubble of record.bubbles){
        const source=bubble.node,target=record.node,path=document.createElementNS('http://www.w3.org/2000/svg','path');
        let d;
        if(this.mobileLayout){const sx=parseFloat(source.style.left)+source.offsetWidth/2,sy=parseFloat(source.style.top)+source.offsetHeight,tx=parseFloat(target.style.left)+target.offsetWidth/2,ty=parseFloat(target.style.top),mid=(sy+ty)/2;d=`M ${sx} ${sy} C ${sx} ${mid}, ${tx} ${mid}, ${tx} ${ty}`;}
        else{const sx=parseFloat(source.style.left)+source.offsetWidth,sy=parseFloat(source.style.top)+source.offsetHeight/2,tx=parseFloat(target.style.left),ty=parseFloat(target.style.top)+target.offsetHeight/2,mid=(sx+tx)/2;d=`M ${sx} ${sy} C ${mid} ${sy}, ${mid} ${ty}, ${tx} ${ty}`;}
        path.setAttribute('d',d);path.setAttribute('class','team-canvas-wire');path.setAttribute('marker-end','url(#team-canvas-arrow)');path.dataset.target=record.issue.id;this.wires.append(path);
      }
    }
  }
  shortID(id){return String(id||'').replace(/-/g,'').slice(0,8).toUpperCase();}
  focusItem(id){
    const node=this.nodes.querySelector(`[data-issue-id="${CSS.escape(id)}"]`);if(!node)return;
    this.nodes.querySelectorAll('.team-canvas-related').forEach(item=>item.classList.remove('team-canvas-related'));
    node.classList.add('team-canvas-related');
    const linkedIDs=new Set((this.lastRecords?.issues||[]).find(row=>row.issue.id===id)?.bubbles.map(row=>row.input.inputId)||[]);
    for(const inputID of linkedIDs)this.nodes.querySelector(`[data-input-id="${CSS.escape(inputID)}"]`)?.classList.add('team-canvas-related');
    this.wires.querySelectorAll('[data-target]').forEach(path=>path.classList.toggle('team-canvas-related',path.dataset.target===id));
    const p=node._canvasPosition;if(p){const rect=this.viewport.getBoundingClientRect();this.transform.x=rect.width/2-(p.x+node.offsetWidth/2)*this.transform.scale;this.transform.y=rect.height/2-(p.y+node.offsetHeight/2)*this.transform.scale;this.applyTransform();}
  }
  focusMessage(id){
    if(this.mode!=='canvas')return;
    const node=this.nodes.querySelector(`[data-input-id="${CSS.escape(id)}"]`),p=node?._canvasPosition;
    if(!p)return;
    const rect=this.viewport.getBoundingClientRect();
    this.transform.scale=Math.max(this.transform.scale,Math.min(.72,(rect.width-48)/this.canvasWidth));
    this.transform.x=rect.width/2-(p.x+node.offsetWidth/2)*this.transform.scale;
    this.transform.y=rect.height/2-(p.y+node.offsetHeight/2)*this.transform.scale;
    this.userInteracted=true;
    this.applyTransform();
  }
  openDetail(issue,trigger){
    this.detailIssue=issue;this.detailRun=(issue.runs||[])[0]||null;this.detailTrigger=trigger;this.detail.hidden=false;this.detail.classList.add('open');
    this.detail.setAttribute('aria-modal',String(window.matchMedia('(max-width:720px)').matches));
    const requestToken={};this.detailRequestToken=requestToken;
    this.renderDetail(issue);
    const teamID=this.current?.id;
    if(!teamID)return;
    this.request('/api/teams/'+encodeURIComponent(teamID)+'/issues/'+encodeURIComponent(issue.id)).then(detail=>{
      if(this.detailRequestToken!==requestToken||this.detailIssue?.id!==issue.id)return;
      this.detailIssue=detail;this.detailRun=(detail.runs||[])[0]||null;this.renderDetail(detail,true);
    }).catch(error=>{
      if(this.detailRequestToken!==requestToken||this.detailIssue?.id!==issue.id)return;
      const body=document.getElementById('team-canvas-detail-body');
      body.append(this.detailSection('交付证据',[this.t('读取事项详情失败')+' · '+String(error.message||error)]));
    });
  }
  renderDetail(issue,preserveScroll=false){
    document.getElementById('team-canvas-detail-id').textContent='Issue · '+this.shortID(issue.id);
    document.getElementById('team-canvas-detail-title').textContent=issue.title||this.t('团队事项');
    document.getElementById('team-canvas-detail-status').textContent=this.statusLabel(issue);
    const body=document.getElementById('team-canvas-detail-body'),scrollTop=preserveScroll?body.scrollTop:0;body.replaceChildren();
    body.append(this.detailSection('当前着落',[
      teamCanvasOwnerLine(issue,value=>this.t(value),kind=>typeof BridgeI18n!=='undefined'?BridgeI18n.agentLabel(kind):kind),
      this.progress(issue)?this.progress(issue).completed+'/'+this.progress(issue).total+' '+this.t('项已完成'):'进度待同步',
      issue.nextAction?.label?this.t('下一步')+' · '+issue.nextAction.label:'',
    ].filter(Boolean)));
    body.append(this.detailSection('最近回音',[
      issue.lastUpdate?.summary||this.t('暂无最近回音'),
      issue.lastUpdate?.at?new Date(issue.lastUpdate.at).toLocaleString(BridgeI18n.locale()):'',
    ].filter(Boolean)));
    const sources=(issue.inputs||[]).map((input,index)=>'#'+String(index+1).padStart(2,'0')+' · '+input.sourceText);
    if(sources.length)body.append(this.detailSection('关联原话',sources));
    const runs=(issue.runs||[]).map(run=>{
      const progress=this.progress(run),parts=[this.statusLabel(run)];
      if(progress)parts.push(progress.completed+'/'+progress.total+' '+this.t('项已完成'));
      const owner=teamCanvasOwnerLine(run,value=>this.t(value),kind=>typeof BridgeI18n!=='undefined'?BridgeI18n.agentLabel(kind):kind);if(owner)parts.push(owner);
      if(run.nextAction?.label)parts.push(this.t('下一步')+' · '+run.nextAction.label);
      if(run.lastUpdate?.summary)parts.push(this.t('最近回音')+' · '+run.lastUpdate.summary);
      const sections=[this.detailSection('Run · '+this.shortID(run.id),parts)];
      const evidence=teamCanvasDeliveryRows(run,value=>this.t(value));
      const result=run.delivery?.resultSummary||{};
      sections.push(this.detailSection('交付证据',evidence.length?evidence:[this.t(result.truncated?'内容已截断':'暂无可展示的执行结果')]));
      return sections;
    });
    for(const sections of runs)body.append(...sections);
    if(!runs.length)body.append(this.detailSection('交付证据',[this.t('暂无可展示的执行结果')]));
    body.scrollTop=scrollTop;
    const openRunButton=document.getElementById('team-canvas-open-run');
    openRunButton.disabled=!this.detailRun;
    openRunButton.textContent=this.detailRun?this.t('打开 Run 详情'):this.t('暂无关联 Run');
  }
  detailSection(title,rows){
    const section=this.node('section',undefined,'team-canvas-detail-section');section.append(this.node('h3',this.t(title)));
    for(const row of rows)section.append(this.node('p',row));return section;
  }
  closeDetail(){
    if(!this.detail)return;
    this.detailRequestToken=null;
    this.detail.classList.remove('open');this.detail.hidden=true;this.detailIssue=null;this.detailRun=null;this.detail.setAttribute('aria-modal','false');
    this.detailTrigger?.focus?.({preventScroll:true});this.detailTrigger=null;
  }
  fitAll(userInitiated=true,readable=false){
    const rect=this.viewport.getBoundingClientRect();if(!rect.width||!rect.height||!this.canvasWidth||!this.canvasHeight)return;
    const fit=teamCanvasFitTransform(rect.width,rect.height,this.canvasWidth,this.canvasHeight,readable?.9:1);
    if(!fit)return;
    if(readable&&fit.scale<.72){
      fit.scale=Math.min(.72,(rect.width-48)/this.canvasWidth);
      fit.x=(rect.width-this.canvasWidth*fit.scale)/2;
      fit.y=18;
    }
    this.transform=fit;
    if(userInitiated)this.userInteracted=true;
    this.applyTransform();
  }
  zoomAt(scale,clientX,clientY){
    const rect=this.viewport.getBoundingClientRect(),x=clientX===undefined?rect.width/2:clientX-rect.left,y=clientY===undefined?rect.height/2:clientY-rect.top;
    const pointX=(x-this.transform.x)/this.transform.scale,pointY=(y-this.transform.y)/this.transform.scale;
    this.transform.scale=Math.max(.08,Math.min(1.6,scale));
    this.userInteracted=true;
    this.transform.x=x-pointX*this.transform.scale;this.transform.y=y-pointY*this.transform.scale;this.applyTransform();
  }
  applyTransform(){
    const {x,y,scale}=this.transform;this.world.style.transform=`translate(${x}px,${y}px) scale(${scale})`;
    this.zoomLabel.textContent=Math.round(scale*100)+'%';
  }
  startPan(event){
    if(event.button!==0||event.target.closest('.team-canvas-node,.team-canvas-detail,.team-canvas-toolbar,.team-canvas-foot'))return;
    this.drag={type:'pan',pointerId:event.pointerId,startX:event.clientX,startY:event.clientY,x:this.transform.x,y:this.transform.y,moved:false};
    this.viewport.setPointerCapture(event.pointerId);this.viewport.classList.add('dragging');
  }
  bindNodeDrag(node,getData){
    node.addEventListener('pointerdown',event=>{
      if(event.button!==0)return;event.stopPropagation();const data=getData();
      this.drag={type:'node',pointerId:event.pointerId,startX:event.clientX,startY:event.clientY,position:node._canvasPosition||{x:parseFloat(node.style.left)||0,y:parseFloat(node.style.top)||0,key:data.key},node,data,moved:false};
      node.setPointerCapture(event.pointerId);
    });
    node.addEventListener('pointermove',event=>this.movePointer(event));
    node.addEventListener('pointerup',event=>this.endPointer(event));
    node.addEventListener('pointercancel',event=>this.endPointer(event));
  }
  movePointer(event){
    const drag=this.drag;if(!drag||drag.pointerId!==event.pointerId)return;
    const dx=event.clientX-drag.startX,dy=event.clientY-drag.startY;
    if(Math.abs(dx)+Math.abs(dy)>4)drag.moved=true;
    if(!drag.moved)return;
    if(drag.type==='pan'){this.transform.x=drag.x+dx;this.transform.y=drag.y+dy;this.applyTransform();}
    else{
      const x=drag.position.x+dx/this.transform.scale,y=drag.position.y+dy/this.transform.scale;
      drag.node.style.left=x+'px';drag.node.style.top=y+'px';drag.node._canvasPosition={x,y,key:drag.position.key};this.positions.set(drag.position.key,{x,y});this.renderWires(this.lastRecords||[]);
    }
    this.userInteracted=true;
    event.preventDefault();
  }
  endPointer(event){
    if(!this.drag||this.drag.pointerId!==event.pointerId)return;
    if(this.drag.moved)this.suppressClickUntil=Date.now()+240;
    if(this.drag.type==='pan')this.viewport.classList.remove('dragging');
    this.drag=null;
  }
}

if(typeof module!=='undefined'&&module.exports)module.exports={TeamCanvasController,teamCanvasAssociations,teamCanvasLinkedRecords,teamCanvasInputRunID,teamCanvasFitTransform,teamCanvasOwnerLine,teamCanvasDeliveryRows};
