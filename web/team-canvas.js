'use strict';

function normalizeTeamCanvasText(value){
  return String(value||'').trim().replace(/\s+/g,' ');
}

function isTeamCanvasInternalMessage(value){
  return /^\[(?:Team delegation|Team context|Team review)\]/i.test(String(value||'').trim())
    ||/^[（(]网关约定[）)]/.test(String(value||'').trim());
}

function teamCanvasAssociations(messages,inputs,runs,details={}){
  const runByID=new Map((runs||[]).map(run=>[run.id,run]));
  const inputTargets=new Map();
  for(const input of inputs||[]){
    const runIDs=Array.isArray(input.runIds)?input.runIds:[];
    const target=input.targetRunId||runIDs[runIDs.length-1];
    if(!target||!runByID.has(target))continue;
    const text=normalizeTeamCanvasText(input.text),targets=inputTargets.get(text)||new Set();
    targets.add(target);inputTargets.set(text,targets);
  }
  const associations={};
  for(const message of messages||[]){
    const text=normalizeTeamCanvasText(message.text);
    if(!text)continue;
    let target=message.runId&&runByID.has(message.runId)?message.runId:'';
    const recordedTargets=inputTargets.get(text);
    if(!target&&recordedTargets?.size===1)target=[...recordedTargets][0];
    if(!target){
      const candidates=new Set();
      for(const run of runs||[]){
        if(normalizeTeamCanvasText(run.goal)===text)candidates.add(run.id);
        const tasks=details[run.id]?.tasks||[];
        if(tasks.some(task=>normalizeTeamCanvasText(task.prompt)===text))candidates.add(run.id);
      }
      if(candidates.size===1)target=[...candidates][0];
    }
    if(target)associations[message.id]=target;
  }
  return associations;
}

class TeamCanvasController{
  constructor({root,messages,request,openRun,onLoadOlder}){
    Object.assign(this,{root,messages,request,openRun,onLoadOlder});
    this.current=null;this.runs=[];this.inputs=[];this.details=new Map();this.messageRows=[];
    this.positions=new Map();this.mode='canvas';this.generation=0;this.pollTimer=null;this.refreshToken=null;
    this.transform={x:0,y:0,scale:1};this.drag=null;this.detailRun=null;
    this.viewport=document.getElementById('team-canvas-viewport');
    this.conversationBar=document.getElementById('team-conversation-view-bar');
    this.world=document.getElementById('team-canvas-world');
    this.nodes=document.getElementById('team-canvas-nodes');
    this.wires=document.getElementById('team-canvas-wires');
    this.detail=document.getElementById('team-canvas-detail');
    this.summary=document.getElementById('team-canvas-summary');
    this.error=document.getElementById('team-canvas-error');
    this.zoomLabel=document.getElementById('team-canvas-zoom');
    this.onMessageMutation=()=>this.readMessages();
    this.observer=typeof MutationObserver==='undefined'?null:new MutationObserver(this.onMessageMutation);
    this.observer?.observe(messages,{childList:true,subtree:true,characterData:true});
    this.bindControls();
  }
  t(value){return typeof BridgeI18n!=='undefined'?BridgeI18n.t(value):value;}
  node(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;}
  button(text,className,action){const button=this.node('button',text,className);button.type='button';if(action)button.addEventListener('click',action);return button;}
  bindControls(){
    document.querySelectorAll('[data-team-view]').forEach(button=>button.addEventListener('click',()=>this.setMode(button.dataset.teamView)));
    document.getElementById('team-open-canvas').addEventListener('click',()=>this.setMode('canvas'));
    document.getElementById('team-canvas-refresh').addEventListener('click',()=>this.refresh(true));
    document.getElementById('team-canvas-fit').addEventListener('click',()=>this.fitAll());
    document.getElementById('team-canvas-zoom-out').addEventListener('click',()=>this.zoomAt(this.transform.scale/1.2));
    document.getElementById('team-canvas-zoom-in').addEventListener('click',()=>this.zoomAt(this.transform.scale*1.2));
    document.getElementById('team-canvas-older').addEventListener('click',()=>this.onLoadOlder?.());
    document.getElementById('team-canvas-detail-close').addEventListener('click',()=>this.closeDetail());
    document.getElementById('team-canvas-open-run').addEventListener('click',()=>{
      const target=this.detailRun;if(target&&this.openRun)this.openRun(this.current.id,target.id);
    });
    this.viewport.addEventListener('pointerdown',event=>this.startPan(event));
    this.viewport.addEventListener('pointermove',event=>this.movePointer(event));
    this.viewport.addEventListener('pointerup',event=>this.endPointer(event));
    this.viewport.addEventListener('pointercancel',event=>this.endPointer(event));
    this.viewport.addEventListener('wheel',event=>{
      event.preventDefault();
      if(event.ctrlKey||event.metaKey)this.zoomAt(this.transform.scale*(event.deltaY<0?1.08:.92),event.clientX,event.clientY);
      else{this.transform.x-=event.deltaX;this.transform.y-=event.deltaY;this.applyTransform();}
    },{passive:false});
    window.addEventListener('resize',()=>{if(this.root.hidden)return;this.render();});
  }
  open(team){
    clearInterval(this.pollTimer);
    this.generation++;this.current=team||null;this.details.clear();this.positions.clear();this.messageStamp='';this.userInteracted=false;this.transform={x:0,y:0,scale:1};this.refreshToken=null;this.closeDetail();
    if(!team){this.root.hidden=true;this.conversationBar.hidden=true;document.getElementById('chat').classList.remove('team-canvas-active');this.messages.hidden=false;return;}
    this.mode=this.savedMode(team.id);this.root.hidden=this.mode!=='canvas';
    this.messages.hidden=this.mode==='canvas';
    document.getElementById('chat').classList.toggle('team-canvas-active',this.mode==='canvas');
    if(this.mode==='canvas')this.root.parentElement.scrollTop=0;
    this.syncModeButtons();this.readMessages();
    this.refresh();
    this.pollTimer=setInterval(()=>this.refresh(),5000);
  }
  close(){this.open(null);}
  accepted(){this.refresh(true);}
  savedMode(teamID){try{return localStorage.getItem('team-chat-view:'+teamID)==='conversation'?'conversation':'canvas';}catch{return'canvas';}}
  setMode(mode){
    if(!this.current||!['canvas','conversation'].includes(mode))return;
    this.mode=mode;try{localStorage.setItem('team-chat-view:'+this.current.id,mode);}catch{}
    this.root.hidden=mode!=='canvas';this.messages.hidden=mode==='canvas';
    document.getElementById('chat').classList.toggle('team-canvas-active',mode==='canvas');
    this.syncModeButtons();
    if(mode==='canvas'){this.root.parentElement.scrollTop=0;this.readMessages();requestAnimationFrame(()=>this.fitAll(false,true));this.refresh();}
  }
  afterHistoryRead(){
    if(!this.current||this.mode!=='canvas')return;
    this.root.parentElement.scrollTop=0;this.readMessages();this.render();this.fitAll(false,true);
  }
  syncModeButtons(){
    document.querySelectorAll('[data-team-view]').forEach(button=>button.setAttribute('aria-pressed',String(button.dataset.teamView===this.mode)));
    this.conversationBar.hidden=!this.current||this.mode!=='conversation';
  }
  readMessages(){
    if(!this.current)return;
    const rows=[...this.messages.querySelectorAll('.message.user')].map((row,index)=>({
      id:row.dataset.key||'message-'+index,
      text:normalizeTeamCanvasText(row.querySelector('.message-body')?.textContent||''),
      index:index+1,
    })).filter(row=>!isTeamCanvasInternalMessage(row.text));
    const stamp=JSON.stringify(rows);
    if(stamp===this.messageStamp)return;
    this.messageStamp=stamp;this.messageRows=rows;
    if(this.mode==='canvas')this.render();
  }
  async refresh(force=false){
    const team=this.current;if(!team||this.refreshToken)return;
    const token={};this.refreshToken=token;const generation=this.generation;this.error.textContent='';
    try{
      const [runResult,inputResult]=await Promise.allSettled([
        this.request('/api/teams/'+encodeURIComponent(team.id)+'/runs'),
        this.request('/api/teams/'+encodeURIComponent(team.id)+'/inputs?limit=100'),
      ]);
      if(generation!==this.generation||this.current?.id!==team.id)return;
      if(runResult.status==='fulfilled')this.runs=Array.isArray(runResult.value.runs)?runResult.value.runs:[];
      else this.error.textContent=this.t(runResult.reason?.message||'读取 Run 摘要失败');
      this.inputs=inputResult.status==='fulfilled'&&Array.isArray(inputResult.value.inputs)?inputResult.value.inputs:[];
      if(runResult.status==='fulfilled')await this.loadSourceDetails(generation);
      if(generation===this.generation&&this.current?.id===team.id){this.render();this.updateOlderButton();}
    }catch(error){if(generation===this.generation)this.error.textContent=this.t(error.message||'画布刷新失败');}
    finally{if(this.refreshToken===token)this.refreshToken=null;}
  }
  async loadSourceDetails(generation){
    const rows=[...this.runs].sort((a,b)=>Date.parse(b.updatedAt||'')-Date.parse(a.updatedAt||''));
    const active=rows.filter(run=>!['completed','failed','cancelled','stopped','outcome_unknown'].includes(run.status));
    const candidates=[...new Map([...active,...rows.slice(0,16)].map(run=>[run.id,run])).values()];
    await Promise.allSettled(candidates.map(async run=>{
      const cached=this.details.get(run.id);if(cached&&cached.version===run.version)return;
      const detail=await this.request('/api/teams/'+encodeURIComponent(this.current.id)+'/runs/'+encodeURIComponent(run.id));
      if(generation===this.generation)this.details.set(run.id,detail);
    }));
  }
  updateOlderButton(){
    const timeline=typeof chatTimeline!=='undefined'?chatTimeline:null;
    const button=document.getElementById('team-canvas-older');
    button.hidden=!timeline?.hasMore;button.disabled=!!timeline?.busy;
  }
  statusLabel(run){
    const labels=typeof TEAM_RUN_STATUS_LABELS!=='undefined'?TEAM_RUN_STATUS_LABELS:{};
    return this.t(labels[run.status]||run.status||'状态待同步');
  }
  progress(run){
    const counts=run?.taskCounts;if(!counts||typeof counts!=='object')return null;
    let total=0,completed=0;
    for(const [status,value] of Object.entries(counts)){const count=Number(value)||0;total+=count;if(status==='completed')completed=count;}
    return total?{total,completed,percent:Math.round(completed/total*100)}:null;
  }
  buildGroups(){
    const details=Object.fromEntries(this.details.entries());
    const sources=this.collectSources();this.sourceRows=sources;
    const associations=teamCanvasAssociations(sources,this.inputs,this.runs,details);
    const runByID=new Map(this.runs.map(run=>[run.id,run])),groups=new Map();
    for(const message of sources){
      const runID=associations[message.id],key=runID||'pending:'+message.id;
      if(!groups.has(key))groups.set(key,{key,run:runByID.get(runID)||null,messages:[],pending:!runID});
      groups.get(key).messages.push(message);
    }
    const active=this.runs.filter(run=>!['completed','failed','cancelled','stopped','outcome_unknown'].includes(run.status));
    for(const run of active)if(!groups.has(run.id))groups.set(run.id,{key:run.id,run,messages:[],pending:false});
    return [...groups.values()].sort((a,b)=>{
      const ai=a.messages.at(-1)?.index||0,bi=b.messages.at(-1)?.index||0;
      if(!ai)return bi?-1:Date.parse(b.run?.updatedAt||'')-Date.parse(a.run?.updatedAt||'');
      if(!bi)return 1;
      return bi-ai;
    });
  }
  collectSources(){
    const sources=[],seen=new Set(),inputByRun=new Map();
    const add=source=>{
      const text=normalizeTeamCanvasText(source.text);if(!text)return;
      const key=source.inputID?'input:'+source.inputID:'source:'+source.id;
      if(seen.has(key))return;seen.add(key);
      sources.push({...source,text});
    };
    for(const input of this.inputs){
      const runID=input.targetRunId||(Array.isArray(input.runIds)?input.runIds[input.runIds.length-1]:'');
      if(runID){const texts=inputByRun.get(runID)||new Set();texts.add(normalizeTeamCanvasText(input.text));inputByRun.set(runID,texts);}
      add({id:'input:'+input.inputId, inputID:input.inputId, runId:runID, text:input.text,
        acceptedAt:input.acceptedAt, sequence:Number(input.sequence||0), kind:'input'});
    }
    for(const run of this.runs){
      const alreadySourced=(inputByRun.get(run.id)||new Set()).has(normalizeTeamCanvasText(run.goal));
      if(!alreadySourced)add({id:'run-goal:'+run.id,inputID:'run:'+run.id,runId:run.id,text:run.goal,acceptedAt:run.createdAt,kind:'run-goal'});
      const detail=this.details.get(run.id),tasks=Array.isArray(detail?.tasks)?detail.tasks:[];
      for(const [index,task] of tasks.entries()){
        if(!String(task.id||'').startsWith('input-'))continue;
        add({id:'input:'+task.id.slice('input-'.length),inputID:task.id.slice('input-'.length),runId:run.id,
          text:task.prompt,acceptedAt:run.createdAt,taskOrder:index,kind:'input-task'});
      }
    }
    const knownSourceText=new Set(sources.map(source=>normalizeTeamCanvasText(source.text)));
    for(const message of this.messageRows){
      if(knownSourceText.has(normalizeTeamCanvasText(message.text)))continue;
      add({...message,kind:'timeline'});
    }
    sources.sort((a,b)=>{
      const atA=Date.parse(a.acceptedAt||'')||0,atB=Date.parse(b.acceptedAt||'')||0;
      if(atA!==atB)return atA-atB;
      if(a.runId&&a.runId===b.runId&&(a.taskOrder!==undefined||b.taskOrder!==undefined))return(a.taskOrder??-1)-(b.taskOrder??-1);
      if(a.sequence||b.sequence)return(a.sequence||0)-(b.sequence||0);
      return(a.order||0)-(b.order||0);
    });
    sources.forEach((source,index)=>{source.index=index+1;});
    return sources;
  }
  render(){
    if(this.root.hidden||this.mode!=='canvas')return;
    this.nodes.replaceChildren();this.wires.replaceChildren();
    const groups=this.buildGroups(),allMessages=groups.reduce((count,group)=>count+group.messages.length,0);
    const linked=groups.filter(group=>group.run).length,pending=groups.filter(group=>group.pending).length;
    this.summary.textContent=[allMessages+' '+this.t('条气泡'),linked+' '+this.t('个事项'),pending+' '+this.t('待关联')].join(' · ');
    const records=[];
    for(const group of groups){
      const messageNodes=[];
      for(const message of group.messages){
        const node=this.renderMessage(message,group);node.style.width='320px';this.nodes.append(node);messageNodes.push({message,node});
      }
      let issueNode=null;
      if(group.run){issueNode=this.renderRun(group.run);issueNode.style.width='360px';this.nodes.append(issueNode);}
      else if(group.pending){issueNode=this.renderPending(group.messages[0]);issueNode.style.width='360px';this.nodes.append(issueNode);}
      records.push({group,messageNodes,issueNode});
    }
    this.lastRecords=records;this.layout(records);
    this.renderWires(records);
    this.updateOlderButton();
    if(!this.userInteracted)requestAnimationFrame(()=>this.fitAll(false,true));
  }
  renderMessage(message,group){
    const button=this.button('', 'team-canvas-node team-canvas-message');button.dataset.messageId=message.id;
    const head=this.node('div',undefined,'team-canvas-message-head');
    head.append(this.node('span',String(message.index).padStart(2,'0'),'team-canvas-message-index'));
    if(message.acceptedAt){
      const at=new Date(message.acceptedAt);
      if(Number.isFinite(at.getTime()))head.append(this.node('time',at.toLocaleTimeString(BridgeI18n.locale(),{hour:'2-digit',minute:'2-digit'}),'team-canvas-message-time'));
    }
    const relation=group.run?this.t('已关联事项')+' · '+this.shortID(group.run.id):this.t('待关联');
    head.append(this.node('span',relation,'team-canvas-message-relation'));
    button.title=message.text||this.t('附件消息');
    button.append(head,this.node('p',message.text||this.t('附件消息'),'team-canvas-message-text'));
    button.setAttribute('aria-label','#'+message.index+' · '+(message.text||this.t('附件消息')));
    button.addEventListener('click',()=>{if(Date.now()<this.suppressClickUntil)return;if(group.run)this.focusItem(group.run.id);});
    this.bindNodeDrag(button,()=>({key:'m:'+message.id,group,record:message}));
    return button;
  }
  renderRun(run){
    const button=this.button('', 'team-canvas-node team-canvas-issue');button.dataset.runId=run.id;
    const head=this.node('div',undefined,'team-canvas-issue-head');
    head.append(this.node('span',this.t('事项')+' · '+this.shortID(run.id),'team-canvas-issue-id'));
    head.append(this.node('span',this.statusLabel(run),'team-canvas-status'));button.append(head);
    button.append(this.node('strong',run.goal||this.t('团队事项'),'team-canvas-issue-title'));
    const progress=this.progress(run);
    if(progress){
      const progressBox=this.node('div',undefined,'team-canvas-progress');
      const progressHead=this.node('div',undefined,'team-canvas-progress-head');
      progressHead.append(this.node('strong',progress.completed+'/'+progress.total+' '+this.t('项已完成')));
      progressHead.append(this.node('span',progress.percent+'%'));
      const track=this.node('div',undefined,'team-canvas-progress-track'),fill=this.node('span',undefined,'team-canvas-progress-fill');fill.style.width=progress.percent+'%';track.append(fill);progressBox.append(progressHead,track);button.append(progressBox);
    }else button.append(this.node('p',this.t('进度待同步'),'team-canvas-issue-muted'));
    const bits=[];
    if(run.currentOwner?.name)bits.push(this.t('当前执行人')+' · '+run.currentOwner.name);
    if(run.lastUpdate?.summary)bits.push(this.t('最近回音')+' · '+run.lastUpdate.summary);
    if(run.nextAction?.label)bits.push(this.t('下一步')+' · '+run.nextAction.label);
    const meta=this.node('div',undefined,'team-canvas-issue-meta');
    for(const bit of bits)meta.append(this.node('span',bit));
    button.append(meta,this.node('span',this.t('查看事项详情'),'team-canvas-open-hint'));
    button.setAttribute('aria-label',this.shortID(run.id)+' '+(run.goal||'')+' · '+this.statusLabel(run)+' · '+this.t('查看事项详情'));
    button.addEventListener('click',()=>{if(Date.now()>=(this.suppressClickUntil||0))this.openDetail(run.id,button);});
    this.bindNodeDrag(button,()=>({key:'r:'+run.id,run}));
    return button;
  }
  renderPending(message){
    const node=this.node('article',undefined,'team-canvas-node team-canvas-pending');
    node.append(this.node('div',this.t('待关联'),'team-canvas-pending-title'));
    node.append(this.node('p',this.t('尚未找到对应的 Run；刷新后会重新核对关联。'),'team-canvas-pending-copy'));
    node.setAttribute('aria-label',this.t('原话')+' '+message.index+' · '+this.t('待关联'));
    return node;
  }
  layout(records){
    const mobile=window.matchMedia('(max-width:720px)').matches;
    if(this.mobileLayout!==undefined&&this.mobileLayout!==mobile)this.positions.clear();
    let width=mobile?380:Math.max(940,Math.min(1880,document.getElementById('team-canvas-viewport').clientWidth*1.55)),height=0;
    const setPosition=(node,key,x,y)=>{
      const override=this.positions.get(key);if(override){x=override.x;y=override.y;}
      node.style.left=x+'px';node.style.top=y+'px';node._canvasPosition={x,y,key};
      const box={x,y,width:node.offsetWidth,height:node.offsetHeight};return box;
    };
    if(mobile){
      let y=28;
      for(const record of records){
        for(const {message,node} of record.messageNodes){const box=setPosition(node,'m:'+message.id,18,y);y+=box.height+12;}
        if(record.issueNode){const key=record.group.run?'r:'+record.group.run.id:'p:'+record.group.messages[0]?.id;const box=setPosition(record.issueNode,key,18,y+10);y+=box.height+38;}
      }
      height=Math.max(600,y+30);
    }else{
      const columns=records.length>1?2:1,colWidth=900,gapY=42;
      width=Math.max(940,columns*colWidth+30);
      let rowY=28;
      for(let offset=0;offset<records.length;offset+=columns){
        const batch=records.slice(offset,offset+columns),rowHeights=[];
        for(const [column,record] of batch.entries()){
          const x=24+column*colWidth,msgBoxes=[];let messagesHeight=0;
          for(const {message,node} of record.messageNodes){
            const box=setPosition(node,'m:'+message.id,x,rowY+messagesHeight);msgBoxes.push(box);messagesHeight+=box.height+14;
          }
          let issueBox=null;
          if(record.issueNode){const key=record.group.run?'r:'+record.group.run.id:'p:'+record.group.messages[0]?.id;const issueY=rowY+Math.max(0,(messagesHeight-record.issueNode.offsetHeight)/2);issueBox=setPosition(record.issueNode,key,x+430,issueY);}
          rowHeights.push(Math.max(messagesHeight,issueBox?.height||0,240));
        }
        rowY+=Math.max(...rowHeights)+gapY;
      }
      height=Math.max(620,rowY+20);
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
    const center=node=>({x:parseFloat(node.style.left)+node.offsetWidth/2,y:parseFloat(node.style.top)+node.offsetHeight/2});
    for(const record of records){
      if(!record.issueNode)continue;
      const target=center(record.issueNode);
      for(const {node} of record.messageNodes){
        const source=center(node),path=document.createElementNS('http://www.w3.org/2000/svg','path');
        let d;
        if(this.mobileLayout){const startY=parseFloat(node.style.top)+node.offsetHeight,endY=parseFloat(record.issueNode.style.top),mid=(startY+endY)/2;d=`M ${source.x} ${startY} C ${source.x} ${mid}, ${target.x} ${mid}, ${target.x} ${endY}`;}
        else{const startX=parseFloat(node.style.left)+node.offsetWidth,endX=parseFloat(record.issueNode.style.left),mid=(startX+endX)/2;d=`M ${startX} ${source.y} C ${mid} ${source.y}, ${mid} ${target.y}, ${endX} ${target.y}`;}
        path.setAttribute('d',d);path.setAttribute('class',record.group.pending?'team-canvas-wire pending':'team-canvas-wire');path.setAttribute('marker-end','url(#team-canvas-arrow)');path.dataset.target=record.group.key;this.wires.append(path);
      }
    }
  }
  shortID(id){return String(id||'').replace(/-/g,'').slice(0,8).toUpperCase();}
  focusItem(id){
    const node=this.nodes.querySelector(`[data-run-id="${CSS.escape(id)}"]`);if(!node)return;
    this.nodes.querySelectorAll('.team-canvas-related').forEach(item=>item.classList.remove('team-canvas-related'));
    node.classList.add('team-canvas-related');
    this.wires.querySelectorAll('[data-target]').forEach(path=>path.classList.toggle('team-canvas-related',path.dataset.target===id));
    const p=node._canvasPosition;if(p){const rect=this.viewport.getBoundingClientRect();this.transform.x=rect.width/2-(p.x+node.offsetWidth/2)*this.transform.scale;this.transform.y=rect.height/2-(p.y+node.offsetHeight/2)*this.transform.scale;this.applyTransform();}
  }
  async openDetail(runID,trigger){
    const run=this.runs.find(row=>row.id===runID);if(!run)return;
    this.detailRun=run;this.detailTrigger=trigger;this.detail.hidden=false;this.detail.classList.add('open');
    this.detail.setAttribute('aria-modal',String(window.matchMedia('(max-width:720px)').matches));
    document.getElementById('team-canvas-detail-id').textContent='Run · '+this.shortID(run.id);
    document.getElementById('team-canvas-detail-title').textContent=run.goal||this.t('团队事项');
    document.getElementById('team-canvas-detail-status').textContent=this.statusLabel(run);
    const body=document.getElementById('team-canvas-detail-body');body.replaceChildren();
    body.append(this.detailSection('当前着落',[
      run.currentOwner?.name?this.t('当前执行人')+' · '+run.currentOwner.name:'',
      this.progress(run)?this.progress(run).completed+'/'+this.progress(run).total+' '+this.t('项已完成'):'进度待同步',
      run.nextAction?.label?this.t('下一步')+' · '+run.nextAction.label:'',
    ].filter(Boolean)));
    body.append(this.detailSection('最近回音',[
      run.lastUpdate?.summary||this.t('暂无最近回音'),
      run.lastUpdate?.at?new Date(run.lastUpdate.at).toLocaleString(BridgeI18n.locale()):'',
    ].filter(Boolean)));
    const sources=(this.sourceRows||[]).filter(message=>teamCanvasAssociations(this.sourceRows||[],this.inputs,this.runs,Object.fromEntries(this.details.entries()))[message.id]===run.id);
    if(sources.length)body.append(this.detailSection('关联原话',sources.map(message=>'#'+String(message.index).padStart(2,'0')+' · '+message.text)));
    body.append(this.node('p',this.t('正在读取任务与交付证据…'),'team-canvas-detail-loading'));
    try{
      const detail=await this.request('/api/teams/'+encodeURIComponent(this.current.id)+'/runs/'+encodeURIComponent(run.id));
      if(this.detailRun?.id!==run.id)return;
      this.details.set(run.id,detail);body.replaceChildren();
      body.append(this.detailSection('当前着落',[
        detail.currentOwner?.name?this.t('当前执行人')+' · '+detail.currentOwner.name:'',
        this.progress(run)?this.progress(run).completed+'/'+this.progress(run).total+' '+this.t('项已完成'):'进度待同步',
        detail.nextAction?.label?this.t('下一步')+' · '+detail.nextAction.label:'',
      ].filter(Boolean)));
      body.append(this.detailSection('最近回音',[
        detail.lastUpdate?.summary||this.t('暂无最近回音'),
        detail.lastUpdate?.at?new Date(detail.lastUpdate.at).toLocaleString(BridgeI18n.locale()):'',
      ].filter(Boolean)));
      const currentSources=(this.sourceRows||[]).filter(message=>teamCanvasAssociations(this.sourceRows||[],this.inputs,this.runs,Object.fromEntries(this.details.entries()))[message.id]===run.id);
      if(currentSources.length)body.append(this.detailSection('关联原话',currentSources.map(message=>'#'+String(message.index).padStart(2,'0')+' · '+message.text)));
      const delivery=detail.delivery||{},verify=delivery.verify;
      const evidence=[];
      if(delivery.agent&&typeof delivery.agent.completed==='number')evidence.push(this.t('Agent 任务')+' '+delivery.agent.completed+'/'+delivery.agent.total);
      if(verify)evidence.push(this.t('验证')+' · '+(verify.passed===true?this.t('通过'):verify.failed?this.t('失败'):verify.configured===false?this.t('未配置'):this.t('等待结果')));
      if(typeof delivery.pendingQuestions==='number')evidence.push(this.t('待回答问题')+' '+delivery.pendingQuestions);
      if(evidence.length)body.append(this.detailSection('交付证据',evidence));
      const tasks=Array.isArray(detail.tasks)?detail.tasks:[];
      if(tasks.length){
        const section=this.node('section',undefined,'team-canvas-detail-section');section.append(this.node('h3',this.t('任务进度')));
        for(const task of tasks.slice(0,24)){
          const row=this.node('div',undefined,'team-canvas-task');
          row.append(this.node('strong',task.instructionsHead||task.prompt||task.id||this.t('任务')));
          row.append(this.node('small',this.t(typeof TEAM_TASK_STATUS_LABELS!=='undefined'?TEAM_TASK_STATUS_LABELS[task.status]||task.status:task.status||'状态待同步')));
          if(task.error)row.append(this.node('p',task.error,'team-canvas-task-error'));
          else if(typeof task.result==='string'&&task.result.trim())row.append(this.node('p',task.result.trim().slice(0,500),'team-canvas-task-result'));
          section.append(row);
        }
        body.append(section);
      }
    }catch(error){body.append(this.node('p',this.t('读取失败：')+error.message,'error'));}
  }
  detailSection(title,rows){
    const section=this.node('section',undefined,'team-canvas-detail-section');section.append(this.node('h3',this.t(title)));
    for(const row of rows)section.append(this.node('p',row));return section;
  }
  closeDetail(){
    if(!this.detail)return;
    this.detail.classList.remove('open');this.detail.hidden=true;this.detailRun=null;this.detail.setAttribute('aria-modal','false');
    this.detailTrigger?.focus?.({preventScroll:true});this.detailTrigger=null;
  }
  fitAll(userInitiated=true,readable=false){
    const rect=this.viewport.getBoundingClientRect();if(!rect.width||!rect.height||!this.canvasWidth||!this.canvasHeight)return;
    const mobile=this.mobileLayout;
    const scale=Math.min(1,(rect.width-48)/this.canvasWidth,(rect.height-42)/this.canvasHeight);
    const minimum=readable?(mobile ? .76 : .72):(mobile ? .42 : .48);
    this.transform.scale=Math.max(minimum,Math.min(1,scale));
    this.transform.x=(rect.width-this.canvasWidth*this.transform.scale)/2;
    this.transform.y=18;
    if(userInitiated)this.userInteracted=true;
    this.applyTransform();
  }
  zoomAt(scale,clientX,clientY){
    const rect=this.viewport.getBoundingClientRect(),x=clientX===undefined?rect.width/2:clientX-rect.left,y=clientY===undefined?rect.height/2:clientY-rect.top;
    const pointX=(x-this.transform.x)/this.transform.scale,pointY=(y-this.transform.y)/this.transform.scale;
    this.transform.scale=Math.max(.38,Math.min(1.6,scale));
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

if(typeof module!=='undefined'&&module.exports)module.exports={TeamCanvasController,teamCanvasAssociations,isTeamCanvasInternalMessage,normalizeTeamCanvasText};
