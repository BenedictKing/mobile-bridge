'use strict';

// API 对象的字段顺序不稳定；数组元素顺序仍参与比较。
function teamCanvasDataEqual(left,right){
  if(left===right)return true;
  if(!left||!right||typeof left!=='object'||typeof right!=='object'||Array.isArray(left)!==Array.isArray(right))return false;
  const keys=Object.keys(left);
  return keys.length===Object.keys(right).length&&keys.every(key=>Object.prototype.hasOwnProperty.call(right,key)&&teamCanvasDataEqual(left[key],right[key]));
}

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
    .map(bubble=>byInputID.get(bubble.input.inputId||bubble.input.id)).filter((bubble,index,rows)=>bubble&&rows.indexOf(bubble)===index)}));
}

function teamCanvasInputRunID(input,issueIDs){
  if(issueIDs?.length)return'';
  const runIDs=Array.isArray(input?.runIds)?input.runIds:[];
  return runIDs[runIDs.length-1]||'';
}

function teamCanvasFitTransform(viewportWidth,viewportHeight,canvasWidth,canvasHeight,maxScale=1){
  if(viewportWidth<=48||viewportHeight<=48||canvasWidth<=0||canvasHeight<=0)return null;
  const scale=Math.min(maxScale,(viewportWidth-48)/canvasWidth,(viewportHeight-48)/canvasHeight);
  return{scale,x:(viewportWidth-canvasWidth*scale)/2,y:(viewportHeight-canvasHeight*scale)/2};
}

// 以关联关系分组，同一输入或事项只出现一次；组和组内气泡按原始输入顺序排列。
function teamCanvasGroups(bubbleRecords,issueRecords){
  const linked=teamCanvasLinkedRecords(bubbleRecords,issueRecords),byBubble=new Map(bubbleRecords.map(record=>[record,[]]));
  for(const issue of linked)for(const bubble of issue.bubbles)byBubble.get(bubble).push(issue);
  const seenBubbles=new Set(),seenIssues=new Set(),groups=[];
  for(const first of bubbleRecords){
    if(seenBubbles.has(first))continue;
    const group={bubbles:[],issues:[]},queue=[first];seenBubbles.add(first);
    for(let index=0;index<queue.length;index++){
      const bubble=queue[index];group.bubbles.push(bubble);
      for(const issue of byBubble.get(bubble)){
        if(seenIssues.has(issue))continue;
        seenIssues.add(issue);group.issues.push(issue);
        for(const source of issue.bubbles)if(!seenBubbles.has(source)){seenBubbles.add(source);queue.push(source);}
      }
    }
    group.bubbles.sort((a,b)=>bubbleRecords.indexOf(a)-bubbleRecords.indexOf(b));
    group.issues.sort((a,b)=>Math.min(...a.bubbles.map(source=>bubbleRecords.indexOf(source)))-Math.min(...b.bubbles.map(source=>bubbleRecords.indexOf(source)))||linked.indexOf(a)-linked.indexOf(b));
    groups.push(group);
  }
  const orphans=linked.filter(issue=>!seenIssues.has(issue));
  if(orphans.length)groups.push({bubbles:[],issues:orphans});
  return groups;
}

function teamCanvasLayout(bubbleRecords,issueRecords,{width=940,mobile=false}={}){
  width=Math.max(mobile?280:700,width);
  const padding=mobile?16:24,gap=32,inner=width-padding*2;
  const lanes=mobile?1:Math.max(1,Math.min(3,Math.floor((inner+gap)/(640+gap))));
  const laneWidth=(inner-gap*(lanes-1))/lanes,positions=[];
  let cursor=padding,rowHeight=0,lane=0;
  for(const group of teamCanvasGroups(bubbleRecords,issueRecords)){
    const full=group.issues.length>1||group.bubbles.length>2;
    if(full&&lane){cursor+=rowHeight+gap;rowHeight=0;lane=0;}
    const groupWidth=full?inner:laneWidth,x=padding+lane*(laneWidth+gap);
    const bubbleWidth=mobile||!group.issues.length?groupWidth:Math.min(320,Math.max(240,groupWidth*.28));
    const issueX=mobile||!group.bubbles.length?0:bubbleWidth+64;
    const available=groupWidth-issueX,columns=mobile?1:Math.max(1,Math.min(group.issues.length,3,Math.floor((available+22)/322)));
    const issueWidth=(available-22*(columns-1))/columns;
    for(const record of group.bubbles)record.node.style.width=bubbleWidth+'px';
    for(const record of group.issues)record.node.style.width=issueWidth+'px';
    const bubbleHeight=group.bubbles.reduce((sum,record)=>sum+record.node.offsetHeight+14,0)-(group.bubbles.length?14:0);
    const issueRows=[];
    for(let index=0;index<group.issues.length;index+=columns){const records=group.issues.slice(index,index+columns);issueRows.push({records,height:Math.max(...records.map(record=>record.node.offsetHeight))});}
    const issueHeight=issueRows.reduce((sum,row)=>sum+row.height+28,0)-(issueRows.length?28:0);
    const height=mobile?bubbleHeight+issueHeight+(bubbleHeight&&issueHeight?28:0):Math.max(bubbleHeight,issueHeight);
    let y=cursor+(mobile?0:(height-bubbleHeight)/2);
    for(const record of group.bubbles){positions.push({record,key:'m:'+(record.input.inputId||record.input.id),x,y});y+=record.node.offsetHeight+14;}
    y=cursor+(mobile?bubbleHeight+(bubbleHeight&&issueHeight?28:0):(height-issueHeight)/2);
    for(const row of issueRows){
      row.records.forEach((record,column)=>positions.push({record,key:'i:'+record.issue.id,x:x+issueX+column*(issueWidth+22),y,grid:columns>1,busX:x+issueX-28}));
      y+=row.height+28;
    }
    rowHeight=Math.max(rowHeight,height);lane+=full?lanes:1;
    if(lane>=lanes){cursor+=rowHeight+gap;rowHeight=0;lane=0;}
  }
  return{positions,width,height:Math.max(600,cursor+rowHeight+padding)};
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
    this.positions=new Map();this.generation=0;this.pollTimer=null;this.refreshToken=null;
    this.transform={x:0,y:0,scale:1};this.drag=null;this.detailIssue=null;this.detailRun=null;this.detailRequestToken=null;this.focusedIssueID=null;
    this.viewport=document.getElementById('team-canvas-viewport');
    this.world=document.getElementById('team-canvas-world');
    this.nodes=document.getElementById('team-canvas-nodes');
    this.wires=document.getElementById('team-canvas-wires');
    this.detail=document.getElementById('team-canvas-detail');
    this.summary=document.getElementById('team-canvas-summary');
    this.error=document.getElementById('team-canvas-error');
    this.zoomLabel=document.getElementById('team-canvas-zoom');
    this.boardRoot=document.getElementById('team-project-board');this.view='canvas';this.boardPanel=null;
    this.touchPoints=new Map();this.pinch=null;
    this.bindControls();
  }
  t(value){return typeof BridgeI18n!=='undefined'?BridgeI18n.t(value):value;}
  node(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;}
  button(text,className,action){const button=this.node('button',text,className);button.type='button';if(action)button.addEventListener('click',action);return button;}
  bindControls(){
    const views=['canvas','board'];
    for(const [index,view] of views.entries()){
      const tab=document.getElementById('team-view-'+view);
      tab.addEventListener('click',()=>this.setView(view));
      tab.addEventListener('keydown',event=>{
        const target={ArrowRight:views[(index+1)%2],ArrowLeft:views[(index+1)%2],Home:'canvas',End:'board'}[event.key];
        if(!target)return;event.preventDefault();this.setView(target);document.getElementById('team-view-'+target).focus();
      });
    }
    document.getElementById('team-canvas-refresh').addEventListener('click',()=>this.refresh(true));
    document.getElementById('team-canvas-fit').addEventListener('click',()=>this.fitAll());
    document.getElementById('team-canvas-zoom-out').addEventListener('click',()=>this.zoomAt(this.transform.scale/1.2));
    document.getElementById('team-canvas-zoom-in').addEventListener('click',()=>this.zoomAt(this.transform.scale*1.2));
    document.getElementById('team-canvas-detail-close').addEventListener('click',()=>this.closeDetail());
    this.detail.addEventListener('keydown',event=>{
      if(event.key==='Escape'){event.preventDefault();event.stopPropagation();this.closeDetail();}
      else if(event.key==='Tab'&&this.detail.getAttribute('aria-modal')==='true'){
        const targets=[...this.detail.querySelectorAll('button:not(:disabled),a[href],[tabindex="0"]')].filter(node=>node.offsetWidth&&node.offsetHeight);
        const first=targets[0],last=targets[targets.length-1];
        if(event.shiftKey&&document.activeElement===first){event.preventDefault();last?.focus();}
        else if(!event.shiftKey&&document.activeElement===last){event.preventDefault();first?.focus();}
      }
    });
    document.getElementById('team-canvas-delete-issue').addEventListener('click',()=>this.deleteIssue());
    document.getElementById('team-canvas-open-run').addEventListener('click',()=>{
      const target=this.detailRun;if(target&&this.openRun)this.openRun(this.current.id,target.id);
    });
    this.viewport.addEventListener('pointerdown',event=>this.startTouch(event),{capture:true});
    this.viewport.addEventListener('pointermove',event=>this.moveTouch(event),{capture:true,passive:false});
    this.viewport.addEventListener('pointerup',event=>this.endTouch(event),{capture:true});
    this.viewport.addEventListener('pointercancel',event=>this.endTouch(event),{capture:true});
    this.viewport.addEventListener('pointerdown',event=>this.startPan(event));
    this.viewport.addEventListener('pointermove',event=>this.movePointer(event));
    this.viewport.addEventListener('pointerup',event=>this.endPointer(event));
    this.viewport.addEventListener('pointercancel',event=>this.endPointer(event));
    this.viewport.addEventListener('wheel',event=>{
      if(event.target.closest('.team-canvas-detail'))return;
      event.preventDefault();
      if(event.ctrlKey||event.metaKey)this.zoomAt(this.transform.scale*(event.deltaY<0?1.08:.92),event.clientX,event.clientY);
      else{this.transform.x-=event.deltaX;this.transform.y-=event.deltaY;this.userInteracted=true;this.applyTransform();}
    },{passive:false});
    window.addEventListener('resize',()=>{if(this.root.hidden)return;this.render();});
  }
  open(team){
    clearInterval(this.pollTimer);
    this.resetTouch();
    this.boardPanel?.stopPolling();this.boardPanel=null;this.boardRoot.replaceChildren();
    this.generation++;this.current=team||null;this.inputs=[];this.issues=[];this.lastRecords=null;this.positions.clear();this.userInteracted=false;this.focusedIssueID=null;this.transform={x:0,y:0,scale:1};this.refreshToken=null;this.refreshQueued=false;this.closeDetail();
    this.nodes.replaceChildren();this.wires.replaceChildren();this.summary.textContent='';
    if(!team){this.root.hidden=true;document.getElementById('chat').classList.remove('team-canvas-active');return;}
    this.root.hidden=false;
    document.getElementById('chat').classList.add('team-canvas-active');
    if(this.viewport.closest('.timeline'))this.viewport.closest('.timeline').scrollTop=0;
    const boardContent=this.node('div');this.boardRoot.append(boardContent);
    this.boardPanel=new TeamsPanel({root:boardContent,request:this.request,teamId:team.id,
      openRun:(teamID,runID)=>this.openRun?.(teamID,runID)});
    let view='canvas';try{view=localStorage.getItem('team-view:'+team.id)||view;}catch{}
    this.setView(view,false);
    this.pollTimer=setInterval(()=>this.refresh(),5000);
  }
  close(){this.open(null);}
  setView(view,persist=true){
    this.resetTouch();
    this.view=view==='board'?'board':'canvas';
    const board=this.view==='board';
    this.closeDetail();this.viewport.hidden=board;this.boardRoot.hidden=!board;this.summary.hidden=board;
    for(const key of ['canvas','board']){
      const tab=document.getElementById('team-view-'+key),selected=this.view===key;
      tab.setAttribute('aria-selected',String(selected));tab.tabIndex=selected?0:-1;
    }
    for(const id of ['team-canvas-fit','team-canvas-zoom-out','team-canvas-zoom-in','team-canvas-zoom'])document.getElementById(id).hidden=board;
    if(persist&&this.current)try{localStorage.setItem('team-view:'+this.current.id,this.view);}catch{}
    if(board)this.boardPanel?.refreshAll();
    else{this.render();this.refresh(true);}
  }
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
  afterHistoryRead(){
    if(!this.current)return;
    if(typeof chatTimeline!=='undefined')chatTimeline?.setViewportLocked(true);
    if(this.viewport.closest('.timeline'))this.viewport.closest('.timeline').scrollTop=0;
    requestAnimationFrame(()=>{if(!this.userInteracted)this.fitAll(false,true);});
  }
  async refresh(force=false){
    const team=this.current;if(!team)return;
    if(this.root?.closest?.('[hidden]'))return;
    if(this.view==='board'){await this.boardPanel?.refreshAll();return;}
    if(this.refreshToken){if(force)this.refreshQueued=true;return;}
    const token={};this.refreshToken=token;const generation=this.generation;this.error.textContent='';
    try{
      const [issueResult,inputResult]=await Promise.allSettled([
        this.request('/api/teams/'+encodeURIComponent(team.id)+'/issues'),
        this.loadInputs(team.id,generation),
      ]);
      if(generation!==this.generation||this.current?.id!==team.id)return;
      let changed=!this.lastRecords;
      if(issueResult.status==='fulfilled'){
        const issues=Array.isArray(issueResult.value.issues)?issueResult.value.issues:[];
        if(!teamCanvasDataEqual(issues,this.issues)){this.issues=issues;changed=true;}
      }else this.error.textContent=this.t(issueResult.reason?.message||'读取 Issue 失败');
      if(inputResult.status==='fulfilled'){
        if(!teamCanvasDataEqual(inputResult.value,this.inputs)){this.inputs=inputResult.value;changed=true;}
      }else this.error.textContent=this.t(inputResult.reason?.message||'读取输入失败');
      if(generation===this.generation&&this.current?.id===team.id&&changed)this.render();
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
    const issues=this.issues.map(issue=>({issue,bubbles:(issue.inputs||[]).map(source=>inputByID.get(source.inputId)).filter(record=>record&&record.issueIDs.includes(issue.id))}));
    return{bubbles,issues};
  }
  render(){
    if(this.root.hidden||this.view==='board')return;
    this.nodes.replaceChildren();this.wires.replaceChildren();
    const {bubbles,issues}=this.buildGroups();
    const pending=bubbles.filter(({input})=>input.associationStatus==='pending').length;
    const summary=[bubbles.length+' '+this.t('条气泡'),issues.length+' '+this.t('个事项'),pending+' '+this.t('待关联')];
    this.summary.textContent=summary.join(' · ');
    const bubbleRecords=bubbles.map(record=>{
      const node=this.renderMessage(record);node.style.width='320px';this.nodes.append(node);return{...record,node};
    });
    const issueRecords=issues.map(record=>{
      const node=this.renderIssue(record.issue);node.style.width='360px';this.nodes.append(node);
      return{...record,node};
    });
    this.lastRecords={bubbles:bubbleRecords,issues:teamCanvasLinkedRecords(bubbleRecords,issueRecords)};this.layout(bubbleRecords,this.lastRecords.issues);
    this.renderWires(this.lastRecords);
    if(!this.userInteracted)requestAnimationFrame(()=>{if(!this.userInteracted)this.fitAll(false,true);});
  }
  associationLabel(input,issueIDs){
    const runHint=teamCanvasInputRunID(input,issueIDs)?' · '+this.t('查看 Run'):'';
    if(input.associationStatus==='clarification')return this.t('需要澄清')+(issueIDs.length?' · '+issueIDs.length+' '+this.t('个事项'):runHint);
    if(issueIDs.length)return this.t('已关联')+' · '+issueIDs.length+' '+this.t('个事项');
    const labels={pending:'正在关联',clarification:'需要澄清',ignored:'无需建项',failed:'关联失败',unclassified:'历史输入未归类'};
    return this.t(labels[input.associationStatus]||'关系待同步')+runHint;
  }
  renderMessage(record){
    const {input,index,issueIDs}=record,id=input.inputId||input.id;
    const linkedRunID=teamCanvasInputRunID(input,issueIDs);
    const card=this.node('div',undefined,'team-canvas-node team-canvas-message');card.dataset.inputId=id;
    const button=this.button('', 'team-canvas-message-main');card.append(button);
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
    if(input.associationReason)button.append(this.node('span',input.associationReason,'team-canvas-message-reason'));
    button.setAttribute('aria-label','#'+index+' · '+(input.text||this.t('附件消息'))+' · '+relation);
    card.addEventListener('click',()=>{
      if(Date.now()<(this.suppressClickUntil||0))return;
      if(issueIDs.length){for(const issueID of issueIDs)this.focusItem(issueID);}
      else if(linkedRunID&&this.openRun)this.openRun(this.current.id,linkedRunID);
    });
    if(!issueIDs.length){
      const remove=this.button(this.t('删除消息'),'plain team-canvas-delete-input',event=>{event.stopPropagation();this.deleteInput(input,remove);});
      remove.setAttribute('aria-label',this.t('删除消息')+' #'+String(input.sequence||index).padStart(2,'0'));
      remove.addEventListener('pointerdown',event=>event.stopPropagation());
      card.append(remove);
    }
    this.bindNodeDrag(card,()=>({key:'m:'+id,record}));
    return card;
  }
  async deleteInput(input,button){
    const team=this.current;if(!team||button.disabled)return;
    if(!window.confirm(this.t('删除这条消息？Run 执行记录保留。')+'\n'+(input.text||this.t('附件消息'))))return;
    button.disabled=true;
    try{
      await this.request('/api/teams/'+encodeURIComponent(team.id)+'/inputs/'+encodeURIComponent(input.inputId||input.id),{method:'DELETE',body:{version:input.version}});
      if(this.current?.id!==team.id)return;
      this.notify?.(this.t('消息已删除'));await this.refresh(true);
    }catch(error){if(this.current?.id===team.id)this.error.textContent=error.message||this.t('删除消息失败');}
    finally{button.disabled=false;}
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
    const plan=teamCanvasLayout(bubbleRecords,issueRecords,{width:this.viewport.clientWidth||940,mobile});
    let {width,height}=plan;
    for(const {record,key,grid,busX,...position} of plan.positions){
      const node=record.node;let {x,y}=position;
      const override=this.positions.get(key);if(override){x=override.x;y=override.y;}
      node.style.left=x+'px';node.style.top=y+'px';node._canvasPosition={x,y,key};
      node._canvasGrid=grid;node._canvasBusX=busX;
      const padding=mobile?16:24;
      width=Math.max(width,x+node.offsetWidth+padding);height=Math.max(height,y+node.offsetHeight+padding);
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
        else if(target._canvasGrid){
          // 多列卡片从行间通道接入顶部，避免连线穿过前一列卡片。
          const sx=parseFloat(source.style.left)+source.offsetWidth,sy=parseFloat(source.style.top)+source.offsetHeight/2;
          const tx=parseFloat(target.style.left)+target.offsetWidth/2,ty=parseFloat(target.style.top),bus=target._canvasBusX,lane=ty-12;
          d=`M ${sx} ${sy} C ${bus} ${sy}, ${bus} ${lane}, ${bus} ${lane} L ${tx-6} ${lane} Q ${tx} ${lane}, ${tx} ${lane+6} L ${tx} ${ty}`;
        }
        else{const sx=parseFloat(source.style.left)+source.offsetWidth,sy=parseFloat(source.style.top)+source.offsetHeight/2,tx=parseFloat(target.style.left),ty=parseFloat(target.style.top)+target.offsetHeight/2,mid=(sx+tx)/2;d=`M ${sx} ${sy} C ${mid} ${sy}, ${mid} ${ty}, ${tx} ${ty}`;}
        path.setAttribute('d',d);path.setAttribute('class','team-canvas-wire');path.setAttribute('marker-end','url(#team-canvas-arrow)');path.dataset.target=record.issue.id;this.wires.append(path);
      }
    }
    this.applyFocus();
  }
  shortID(id){return String(id||'').replace(/-/g,'').slice(0,8).toUpperCase();}
  activeRuns(issue){
    const terminal=typeof TEAM_TERMINAL_RUN_STATUSES!=='undefined'?TEAM_TERMINAL_RUN_STATUSES:new Set(['completed','failed','cancelled','stopped','outcome_unknown']);
    return [...new Map((issue.runs||[]).filter(run=>run.id&&!terminal.has(run.status)).map(run=>[run.id,run])).values()];
  }
  applyFocus(){
    const id=this.focusedIssueID;
    this.nodes.querySelectorAll('.team-canvas-related').forEach(item=>item.classList.remove('team-canvas-related'));
    this.wires.querySelectorAll('[data-target]').forEach(path=>path.classList.toggle('team-canvas-related',!!id&&path.dataset.target===id));
    if(!id)return;
    this.nodes.querySelector(`[data-issue-id="${CSS.escape(id)}"]`)?.classList.add('team-canvas-related');
    const linkedIDs=new Set((this.lastRecords?.issues||[]).find(row=>row.issue.id===id)?.bubbles.map(row=>row.input.inputId||row.input.id)||[]);
    for(const inputID of linkedIDs)this.nodes.querySelector(`[data-input-id="${CSS.escape(inputID)}"]`)?.classList.add('team-canvas-related');
  }
  focusItem(id){
    const node=this.nodes.querySelector(`[data-issue-id="${CSS.escape(id)}"]`);if(!node)return;
    this.focusedIssueID=id;this.applyFocus();this.userInteracted=true;
    const related=[node,...((this.lastRecords?.issues||[]).find(row=>row.issue.id===id)?.bubbles||[]).map(row=>row.node)];
    const boxes=related.filter(item=>item?._canvasPosition).map(item=>({...item._canvasPosition,width:item.offsetWidth,height:item.offsetHeight}));
    if(!boxes.length)return;
    const left=Math.min(...boxes.map(box=>box.x)),top=Math.min(...boxes.map(box=>box.y));
    const width=Math.max(...boxes.map(box=>box.x+box.width))-left,height=Math.max(...boxes.map(box=>box.y+box.height))-top;
    const rect=this.viewport.getBoundingClientRect();
    // 桌面详情占据右侧，关联节点应完整落在剩余画布；手机全屏详情关闭后再看画布。
    const detailWidth=!this.mobileLayout&&!this.detail.hidden?this.detail.offsetWidth:0;
    const fit=teamCanvasFitTransform(rect.width-detailWidth,rect.height,width,height);
    if(fit){this.transform={scale:fit.scale,x:fit.x-left*fit.scale,y:fit.y-top*fit.scale};this.applyTransform();}
  }
  focusMessage(id){
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
    document.getElementById('team-canvas-detail-error').textContent='';
    this.detailIssue=issue;this.detailRun=(issue.runs||[])[0]||null;this.detailTrigger=trigger;this.detail.hidden=false;this.detail.classList.add('open');
    this.detail.setAttribute('aria-modal',String(window.matchMedia('(max-width:720px)').matches));
    this.focusItem(issue.id);
    const requestToken={};this.detailRequestToken=requestToken;
    this.renderDetail(issue);
    document.getElementById('team-canvas-detail-close').focus({preventScroll:true});
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
    const sources=(this.lastRecords?.issues||[]).find(row=>row.issue.id===issue.id)?.bubbles||[];
    if(sources.length){
      const section=this.detailSection('关联原话',[]);
      for(const {input,index} of sources){
        const label='#'+String(input.sequence||index).padStart(2,'0')+' · '+(input.text||this.t('附件消息'));
        section.append(this.button(label,'plain team-canvas-source',()=>{
          this.closeDetail();this.focusMessage(input.inputId||input.id);
        }));
      }
      body.append(section);
    }
    const runs=(issue.runs||[]).map(run=>{
      const progress=this.progress(run),parts=[this.statusLabel(run)];
      if(progress)parts.push(progress.completed+'/'+progress.total+' '+this.t('项已完成'));
      const owner=teamCanvasOwnerLine(run,value=>this.t(value),kind=>typeof BridgeI18n!=='undefined'?BridgeI18n.agentLabel(kind):kind);if(owner)parts.push(owner);
      if(run.nextAction?.label)parts.push(this.t('下一步')+' · '+run.nextAction.label);
      if(run.lastUpdate?.summary)parts.push(this.t('最近回音')+' · '+run.lastUpdate.summary);
      const sections=[this.detailSection('Run · '+this.shortID(run.id),parts)];
      if(this.openRun){
        const teamID=this.current.id;
        sections[0].append(this.button(this.t('打开 Run 详情'),'plain',()=>this.openRun(teamID,run.id)));
      }
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
    if(!this.deletingIssue)document.getElementById('team-canvas-delete-issue').textContent=this.t(this.activeRuns(issue).length?'取消 Run 并删除':'删除事项与对话');
  }
  async deleteIssue(){
    const issue=this.detailIssue,team=this.current;
    if(!issue||!team||this.deletingIssue)return;
    const button=document.getElementById('team-canvas-delete-issue'),errorBox=document.getElementById('team-canvas-detail-error');
    const path='/api/teams/'+encodeURIComponent(team.id)+'/issues/'+encodeURIComponent(issue.id);
    this.deletingIssue=true;button.disabled=true;button.textContent=this.t('正在读取…');errorBox.textContent='';
    let cancelled=0;
    try{
      // 确认前重读关联，不能按打开详情时的旧状态静默取消新增 Run。
      const latest=await this.request(path);
      if(this.current?.id!==team.id||this.detailIssue?.id!==issue.id)return;
      const runs=this.activeRuns(latest);
      const prompt=this.t(runs.length
        ?'将先取消以下未结束的 Run，再删除此事项及关联对话。这些 Run 关联的其他事项也会停止执行；已有文件改动不会回退，执行记录保留。确认取消并删除？'
        :'删除此事项及其关联对话？这些对话与其他事项的关联也会移除；Run 执行记录保留。确认删除？');
      const runLines=runs.map(run=>'Run '+this.shortID(run.id)+' · '+this.statusLabel(run)+' · '+(run.goal||run.id));
      if(!window.confirm([prompt,latest.title||issue.title||this.t('团队事项'),...runLines].join('\n')))return;
      button.textContent=this.t(runs.length?'正在取消并删除…':'正在删除…');
      for(const run of runs){
        const runPath='/api/teams/'+encodeURIComponent(team.id)+'/runs/'+encodeURIComponent(run.id);
        try{await this.request(runPath+'/cancel',{method:'POST',body:{}});cancelled++;}
        catch(error){
          if(error.status!==409)throw error;
          const current=await this.request(runPath);
          if(this.activeRuns({runs:[current]}).length)throw error;
        }
      }
      await this.request(path,{method:'DELETE',body:{}});
      if(this.current?.id!==team.id)return;
      if(this.detailIssue?.id===issue.id)this.closeDetail();
      this.notify?.(this.t('事项及关联对话已删除'));
      await this.refresh(true);
    }catch(error){
      const message=(cancelled?this.t('关联 Run 已取消，但事项尚未删除：'):'')+this.t(error.message||'删除事项失败');
      if(this.current?.id===team.id&&this.detailIssue?.id===issue.id)errorBox.textContent=message;
      else this.notify?.(message);
    }finally{
      this.deletingIssue=false;button.disabled=false;
      button.textContent=this.t(this.detailIssue&&this.activeRuns(this.detailIssue).length?'取消 Run 并删除':'删除事项与对话');
    }
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
    if(readable){
      fit.scale=Math.min(1,rect.width/this.canvasWidth);
      fit.x=(rect.width-this.canvasWidth*fit.scale)/2;
      fit.y=0;
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
  resetTouch(){
    for(const id of this.touchPoints?.keys()||[])if(this.viewport.hasPointerCapture?.(id))this.viewport.releasePointerCapture(id);
    this.touchPoints?.clear();this.pinch=null;this.touchPinched=false;this.drag=null;this.viewport.classList?.remove('dragging');
  }
  startTouch(event){
    if(event.pointerType!=='touch'||event.target.closest('.team-canvas-detail'))return;
    this.touchPoints.set(event.pointerId,{x:event.clientX,y:event.clientY});
    if(this.touchPoints.size<2)return;
    if(!this.pinch)this.beginPinch();
    // 从卡片拖拽接管到画布，避免另一根手指移动卡片或触发点击。
    this.drag=null;this.touchPinched=true;this.viewport.classList.remove('dragging');this.userInteracted=true;
    for(const id of this.touchPoints.keys())this.viewport.setPointerCapture(id);
    this.suppressClickUntil=Date.now()+350;event.preventDefault();event.stopPropagation();
  }
  beginPinch(){
    const [a,b]=[...this.touchPoints.values()],rect=this.viewport.getBoundingClientRect();
    const x=(a.x+b.x)/2-rect.left,y=(a.y+b.y)/2-rect.top;
    this.pinch={distance:Math.max(1,Math.hypot(a.x-b.x,a.y-b.y)),scale:this.transform.scale,
      worldX:(x-this.transform.x)/this.transform.scale,worldY:(y-this.transform.y)/this.transform.scale};
  }
  moveTouch(event){
    if(!this.touchPoints.has(event.pointerId))return;
    this.touchPoints.set(event.pointerId,{x:event.clientX,y:event.clientY});
    if(!this.pinch)return;
    const [a,b]=[...this.touchPoints.values()],rect=this.viewport.getBoundingClientRect(),pinch=this.pinch;
    const scale=Math.max(.08,Math.min(1.6,pinch.scale*Math.hypot(a.x-b.x,a.y-b.y)/pinch.distance));
    this.transform={scale,x:(a.x+b.x)/2-rect.left-pinch.worldX*scale,y:(a.y+b.y)/2-rect.top-pinch.worldY*scale};
    this.applyTransform();this.suppressClickUntil=Date.now()+350;event.preventDefault();event.stopPropagation();
  }
  endTouch(event){
    if(!this.touchPoints.delete(event.pointerId))return;
    if(this.viewport.hasPointerCapture(event.pointerId))this.viewport.releasePointerCapture(event.pointerId);
    if(!this.touchPinched)return;
    this.suppressClickUntil=Date.now()+350;
    if(this.touchPoints.size>=2)this.beginPinch();else this.pinch=null;
    if(!this.touchPoints.size)this.touchPinched=false;
    event.stopPropagation();
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

if(typeof module!=='undefined'&&module.exports)module.exports={TeamCanvasController,teamCanvasAssociations,teamCanvasLinkedRecords,teamCanvasInputRunID,teamCanvasFitTransform,teamCanvasGroups,teamCanvasLayout,teamCanvasOwnerLine,teamCanvasDeliveryRows};
