'use strict';

const TEAM_LIVE_ATTENTION_STATUSES=new Set(['failed','outcome_unknown','stopped']);
const TEAM_LIVE_RUNNING_STATUSES=new Set(['running','dispatching']);
const TEAM_LIVE_KNOWN_STATUSES=new Set(['planned','ready','dispatching','running','completed','failed','cancelled','outcome_unknown','stopped']);

function teamLiveTaskTitle(task,node){
  if(task&&typeof teamWorkflowTaskTitle==='function')return teamWorkflowTaskTitle(task);
  return task?.prompt||task?.instructionsHead||node?.label||node?.id||task?.id||'';
}

function teamMemberRunState(member,run){
  const tasks=(run?.tasks||[]).filter(task=>task?.memberId===member?.id);
  const byId=new Map((run?.tasks||[]).map(task=>[task.id,task]));
  const questions=new Set((run?.pendingQuestions||[]).filter(question=>!question.status||question.status==='open').map(question=>question.taskId));
  const rank=task=>{
    if(questions.has(task.id)||TEAM_LIVE_ATTENTION_STATUSES.has(task.status)||!TEAM_LIVE_KNOWN_STATUSES.has(task.status))return 0;
    if(TEAM_LIVE_RUNNING_STATUSES.has(task.status))return 1;
    if(task.status==='ready')return 2;
    if(task.status==='planned'){
      const blocked=(task.dependsOn||[]).some(id=>byId.get(id)?.status!=='completed');
      return blocked?3:2;
    }
    if(task.status==='completed')return 4;
    return 5;
  };
  const ordered=tasks.map((task,index)=>({task,index,rank:rank(task)})).sort((a,b)=>a.rank-b.rank||a.index-b.index);
  let state='idle';
  if(tasks.length&&tasks.every(task=>task.status==='completed'))state='completed';
  else if(ordered.length)state=['attention','running','ready','waiting','completed','idle'][ordered[0].rank];
  if(!tasks.length&&member?.role==='lead'&&['collecting','reducing'].includes(run?.status))state='running';
  return{state,task:ordered[0]?.task||null,tasks,runningCount:tasks.filter(task=>TEAM_LIVE_RUNNING_STATUSES.has(task.status)).length};
}

function teamGraphRelations(nodes,selection){
  const rows=(nodes||[]).filter(node=>node?.id),byId=new Map(rows.map(node=>[node.id,node]));
  const selected=new Set();
  if(selection?.kind==='task'&&byId.has(selection.id))selected.add(selection.id);
  if(selection?.kind==='member')for(const node of rows){
    const ids=Array.isArray(node.memberIds)?node.memberIds:[node.memberId];
    if(ids.includes(selection.id))selected.add(node.id);
  }
  const downstream=new Map(rows.map(node=>[node.id,[]]));
  for(const node of rows)for(const dependency of node.dependsOn||[])if(downstream.has(dependency))downstream.get(dependency).push(node.id);
  const walk=(direction)=>{
    const visited=new Set(),queue=[...selected];
    while(queue.length){
      const id=queue.shift();
      const next=direction==='upstream'?(byId.get(id)?.dependsOn||[]):(downstream.get(id)||[]);
      for(const related of next){
        if(!byId.has(related)||selected.has(related)||visited.has(related))continue;
        visited.add(related);queue.push(related);
      }
    }
    return visited;
  };
  const upstream=walk('upstream'),downstreamIDs=walk('downstream');
  return{selected,upstream,downstream:downstreamIDs,related:new Set([...selected,...upstream,...downstreamIDs])};
}

function teamSelectionAfterRefresh(selection,nodes){
  if(!selection)return null;
  return teamGraphRelations(nodes,selection).selected.size?selection:null;
}

function teamLiveFallbackNodes(run){
  return(run?.tasks||[]).map(task=>({id:task.id,label:teamLiveTaskTitle(task),status:task.status,memberIds:[task.memberId],taskIds:[task.id],dependsOn:task.dependsOn||[],runCount:1}));
}

function teamLiveNodeTasks(node,run){
  const byId=new Map((run?.tasks||[]).map(task=>[task.id,task]));
  return(node.taskIds||[node.id]).map(id=>byId.get(id)).filter(Boolean);
}

function teamLiveLayers(nodes){
  const byId=new Map(nodes.map(node=>[node.id,node])),remaining=[...byId.keys()],placed=new Set(),layers=[];
  while(remaining.length){
    const ids=remaining.filter(id=>(byId.get(id).dependsOn||[]).filter(dep=>byId.has(dep)).every(dep=>placed.has(dep)));
    if(!ids.length){layers.push(remaining.map(id=>byId.get(id)));break;}
    for(const id of ids){remaining.splice(remaining.indexOf(id),1);placed.add(id);}
    layers.push(ids.map(id=>byId.get(id)));
  }
  return layers;
}

class TeamLivePanelController{
  constructor({root,request,openRun,openSession}){
    Object.assign(this,{root,request,openRun,openSession});
    this.generation=0;this.team=null;this.run=null;this.detail=null;this.graph=null;this.issue=null;
    this.pinned=null;this.hoverTaskId=null;this.focusTaskId=null;this.loading=false;this.error='';this.graphError='';
  }
  t(value){return typeof BridgeI18n!=='undefined'?BridgeI18n.t(value):value;}
  node(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;}
  button(text,className,action){const button=this.node('button',text,className);button.type='button';if(action)button.addEventListener('click',action);return button;}
  runStatus(status){return this.t(typeof teamRunStatusLabel==='function'?teamRunStatusLabel(status):status||'状态待同步');}
  taskStatus(status){return this.t(typeof teamTaskStatusLabel==='function'?teamTaskStatusLabel(status):status||'状态待同步');}
  shortID(id){return String(id||'').replace(/-/g,'').slice(0,8).toUpperCase();}
  member(memberId){return(this.team?.members||[]).find(member=>member.id===memberId)||null;}
  memberLabel(memberId){
    const member=this.member(memberId);
    if(typeof teamWorkflowMemberLabel==='function')return teamWorkflowMemberLabel(member,{memberId});
    return member?.name||member?.id||memberId||this.t('成员');
  }
  open({team,run,issue=null,selection=null}){
    if(!run){this.close();return Promise.resolve();}
    const generation=++this.generation;
    this.team=team;this.run=run;this.issue=issue;this.detail=Array.isArray(run.tasks)?run:null;this.graph=null;
    this.pinned=selection;this.hoverTaskId=null;this.focusTaskId=null;this.error='';this.graphError='';this.loading=true;
    this.loadedVersion=this.detail?.version;
    this.render();
    return this.load(generation);
  }
  update({team,run,issue=null}){
    if(!run)return;
    if(this.run?.id!==run.id||this.team?.id!==team.id){return this.open({team,run,issue});}
    const previousVersion=this.loadedVersion;
    this.team=team;this.run=run;this.issue=issue;
    if(Array.isArray(run.tasks)){this.detail=run;this.loadedVersion=run.version;}
    const changed=run.version!==undefined&&run.version!==previousVersion;
    if(changed||!this.detail){this.loading=!this.detail;this.render();return this.load(++this.generation);}
    if(Array.isArray(run.tasks))this.render();
    return Promise.resolve();
  }
  close(){
    this.generation++;this.team=null;this.run=null;this.detail=null;this.graph=null;this.issue=null;
    this.pinned=null;this.hoverTaskId=null;this.focusTaskId=null;this.root.replaceChildren();
  }
  async load(generation=this.generation){
    const team=this.team,run=this.run;if(!team||!run)return;
    const base='/api/teams/'+encodeURIComponent(team.id)+'/runs/'+encodeURIComponent(run.id);
    const [detailResult,graphResult]=await Promise.allSettled([
      this.request(base),this.request(base+'/graph'),
    ]);
    if(generation!==this.generation||this.run?.id!==run.id||this.team?.id!==team.id)return;
    this.loading=false;
    if(detailResult.status==='fulfilled'){
      this.run=detailResult.value;this.detail=detailResult.value;this.loadedVersion=detailResult.value.version;this.error='';
    }else this.error=detailResult.reason?.message||this.t('读取 Run 详情失败');
    if(graphResult.status==='fulfilled'){this.graph=graphResult.value;this.graphError='';}
    else if(graphResult.reason?.status===404){this.graph=null;this.graphError='';}
    else{this.graph=null;this.graphError=graphResult.reason?.message||this.t('读取任务关系失败');}
    this.pinned=teamSelectionAfterRefresh(this.pinned,this.nodes());
    if(!this.nodes().some(node=>node.id===this.hoverTaskId))this.hoverTaskId=null;
    if(!this.nodes().some(node=>node.id===this.focusTaskId))this.focusTaskId=null;
    this.render();
  }
  nodes(){
    const graphNodes=Array.isArray(this.graph?.nodes)?this.graph.nodes.filter(node=>node?.id):[];
    if(!Array.isArray(this.detail?.tasks))return graphNodes;
    // 旧网关不提供阶段到任务的映射；此时使用任务图，避免把组 ID 当作执行任务。
    const ids=new Set(this.detail.tasks.map(task=>task.id)),mapped=new Set();
    const complete=graphNodes.length&&graphNodes.every(node=>Array.isArray(node.taskIds)&&node.taskIds.length&&node.taskIds.every(id=>{mapped.add(id);return ids.has(id);}));
    return complete&&mapped.size===ids.size?graphNodes:teamLiveFallbackNodes(this.detail);
  }
  effectiveSelection(){
    const preview=this.focusTaskId||this.hoverTaskId;
    return preview?{kind:'task',id:preview}:this.pinned;
  }
  select(kind,id){
    this.pinned=this.pinned?.kind===kind&&this.pinned?.id===id?null:{kind,id};
    this.hoverTaskId=null;this.focusTaskId=null;this.updateSelection();
  }
  preview(taskId,source){
    if(this.rendering)return;
    const key=source==='focus'?'focusTaskId':'hoverTaskId';
    if(this[key]===taskId)return;
    this[key]=taskId;this.updateSelection();
  }
  clearSelection(){
    if(!this.pinned&&!this.hoverTaskId&&!this.focusTaskId)return false;
    this.pinned=null;this.hoverTaskId=null;this.focusTaskId=null;this.updateSelection();return true;
  }
  restoreSelection(selection){this.pinned=teamSelectionAfterRefresh(selection,this.nodes());this.render();}
  progress(){
    const tasks=this.detail?.tasks;
    if(Array.isArray(tasks))return{completed:tasks.filter(task=>task.status==='completed').length,total:tasks.length};
    const counts=this.run?.taskCounts||{};let completed=0,total=0;
    for(const [status,value] of Object.entries(counts)){const count=Number(value)||0;total+=count;if(status==='completed')completed=count;}
    return{completed,total};
  }
  openTaskSession(task){
    if(!task)return;
    const member=this.member(task.memberId),sessionId=typeof teamTaskSessionId==='function'?teamTaskSessionId(task,this.detail,member):(task.result?.sessionId||task.dispatchSessionId||member?.sessionId||'');
    if(sessionId)this.openSession?.(sessionId,{teamId:this.team.id,runId:this.run.id,taskId:task.id,...(task.result?.turnId?{turnId:task.result.turnId}:{})});
  }
  captureFocus(){
    const active=typeof document!=='undefined'?document.activeElement:null;
    if(!this.root.contains?.(active))return null;
    if(active?.dataset?.liveTask)return{kind:'task',id:active.dataset.liveTask,preview:this.focusTaskId===active.dataset.liveTask};
    if(active?.dataset?.liveMember)return{kind:'member',id:active.dataset.liveMember};
    return null;
  }
  restoreFocus(focus){
    if(!focus)return;
    const key=focus.kind==='task'?'liveTask':'liveMember',selector=focus.kind==='task'?'[data-live-task]':'[data-live-member]';
    const target=[...(this.root.querySelectorAll?.(selector)||[])].find(node=>node.dataset[key]===focus.id);
    if(!target)return;
    const rendering=this.rendering;this.rendering=true;
    try{target.focus({preventScroll:true});}finally{this.rendering=rendering;}
    if(focus.preview){this.focusTaskId=focus.id;this.updateSelection();}
  }
  render(){
    if(!this.root||this.rendering)return;
    this.rendering=true;
    try{
      const focus=this.captureFocus();
      const graphScroll=this.root.querySelector?.('.team-live-graph-scroll')?.scrollLeft||0;
      const graphTop=this.root.querySelector?.('.team-live-graph-scroll')?.scrollTop||0;
      this.root.replaceChildren();this.root.className='team-live-panel';this.root.setAttribute('aria-label',this.t('实时 Run'));
      if(!this.run)return;
      const head=this.node('div',undefined,'team-live-head');
      const title=this.node('div',undefined,'team-live-title');
      title.append(this.node('h3',this.t('实时 Run')+' · '+this.shortID(this.run.id)),this.node('span',this.runStatus(this.run.status),'team-live-status'));
      head.append(title);
      const progress=this.progress();head.append(this.node('span',progress.completed+'/'+progress.total+' '+this.t('个任务'),'team-live-progress'));
      this.root.append(head);
      if(this.run.nextAction?.label)this.root.append(this.node('p',this.t('下一步')+' · '+this.run.nextAction.label,'team-live-next'));
      if(this.error)this.root.append(this.node('p',this.t('刷新失败：')+this.error,'error'));
      if(this.loading&&!this.detail){this.root.append(this.node('p',this.t('正在读取任务与交付证据…'),'muted'));return;}
      this.root.append(this.renderMembers(),this.renderGraph(),this.renderSelection());
      const actions=this.node('div',undefined,'team-live-actions');
      actions.append(this.button(this.t('打开完整 Run'),'plain',()=>this.openRun?.(this.team.id,this.run.id,{})));
      if(this.error||this.graphError)actions.append(this.button(this.t('重试'),'plain',()=>{this.loading=true;this.render();this.load(++this.generation);}));
      this.root.append(actions);
      this.restoreFocus(focus);
      const scroller=this.root.querySelector?.('.team-live-graph-scroll');if(scroller){scroller.scrollLeft=graphScroll;scroller.scrollTop=graphTop;}
    }finally{this.rendering=false;}
  }
  updateSelection(){
    const selection=this.effectiveSelection(),relations=teamGraphRelations(this.nodes(),selection);
    for(const card of this.root.querySelectorAll?.('[data-live-task]')||[]){
      const id=card.dataset.liveTask;
      for(const [name,active] of Object.entries({selected:relations.selected.has(id),upstream:relations.upstream.has(id),downstream:relations.downstream.has(id),dimmed:!!selection&&!relations.related.has(id)}))card.classList.toggle(name,active);
      card.setAttribute('aria-pressed',String(this.pinned?.kind==='task'&&this.pinned.id===id));
    }
    for(const card of this.root.querySelectorAll?.('[data-live-member]')||[])card.setAttribute('aria-pressed',String(this.pinned?.kind==='member'&&this.pinned.id===card.dataset.liveMember));
    for(const edge of this.root.querySelectorAll?.('.team-live-graph-edge')||[])edge.setAttribute('class','team-live-graph-edge '+this.edgeClass({from:edge.dataset.from,to:edge.dataset.to},relations));
    this.root.querySelector?.('.team-live-selection')?.replaceWith(this.renderSelection());
  }
  renderMembers(){
    const section=this.node('section',undefined,'team-live-section');section.append(this.node('h3',this.t('实时成员')));
    const members=[...(this.team?.members||[])].sort((a,b)=>Number(b.id===this.team.leadId)-Number(a.id===this.team.leadId));
    if(!members.length)section.append(this.node('p',this.t('暂无成员状态'),'muted'));
    for(const member of members){
      const state=teamMemberRunState(member,this.detail||this.run),labels={attention:'需处理',running:'运行中',ready:'就绪',waiting:'等待依赖',completed:'已完成',idle:'空闲'};
      const row=this.node('div',undefined,'team-live-member');row.dataset.state=state.state;
      const select=this.button('', 'team-live-member-select',()=>this.select('member',member.id));
      select.dataset.liveMember=member.id;select.setAttribute('aria-pressed',String(this.pinned?.kind==='member'&&this.pinned.id===member.id));
      const name=this.node('span',undefined,'team-live-member-name');
      name.append(this.node('strong',this.memberLabel(member.id)),this.node('small',this.t(labels[state.state])+(member.available===false?' · '+this.t('不可用'):'')+(member.enabled===false?' · '+this.t('已停用'):'')));
      select.append(name);
      const taskTitle=state.task?teamLiveTaskTitle(state.task):this.t('当前 Run 没有活动任务');
      select.append(this.node('span',taskTitle+(state.state==='attention'&&state.runningCount?' · '+this.t('另有 ')+state.runningCount+' '+this.t('项运行中'):''),'team-live-member-task'));
      const sessionId=typeof teamTaskSessionId==='function'?teamTaskSessionId(state.task,this.detail,member):member.sessionId;
      const open=this.button(this.t('打开会话'),'plain team-live-member-open',()=>this.openTaskSession(state.task||{memberId:member.id}));
      open.disabled=!sessionId;if(!sessionId)open.title=this.t('该任务尚未绑定执行会话');
      row.append(select,open);section.append(row);
    }
    return section;
  }
  renderGraph(){
    const section=this.node('section',undefined,'team-live-section'),nodes=this.nodes();
    section.append(this.node('h3',this.t('任务关系')));
    if(this.graphError)section.append(this.node('p',this.t('任务图刷新失败，已使用 Run 任务列表。')+this.graphError,'error'));
    if(!nodes.length){section.append(this.node('p',this.t('还没有任务。'),'muted'));return section;}
    const relations=teamGraphRelations(nodes,this.effectiveSelection());
    const compact=typeof globalThis.matchMedia==='function'&&globalThis.matchMedia('(max-width:720px)').matches;
    const layout=!compact&&typeof teamGraphLayout==='function'?teamGraphLayout(nodes,'TB',node=>teamGraphNodeHeight(node)):null;
    if(layout){
      const scroll=this.node('div',undefined,'team-live-graph-scroll');scroll.tabIndex=0;scroll.setAttribute('role','region');scroll.setAttribute('aria-label',this.t('任务关系图'));
      const stage=this.node('div',undefined,'team-live-graph-stage');stage.style.width=layout.width+'px';stage.style.height=layout.height+'px';
      const svg=document.createElementNS('http://www.w3.org/2000/svg','svg');svg.setAttribute('class','team-live-graph-edges');svg.setAttribute('width',layout.width);svg.setAttribute('height',layout.height);svg.setAttribute('viewBox','0 0 '+layout.width+' '+layout.height);svg.setAttribute('aria-hidden','true');
      const defs=document.createElementNS('http://www.w3.org/2000/svg','defs'),marker=document.createElementNS('http://www.w3.org/2000/svg','marker');
      for(const [key,value] of Object.entries({id:'team-live-arrow-'+this.generation,markerWidth:8,markerHeight:8,refX:7,refY:4,orient:'auto'}))marker.setAttribute(key,value);
      const arrow=document.createElementNS('http://www.w3.org/2000/svg','path');arrow.setAttribute('d','M0,0 L8,4 L0,8 z');arrow.setAttribute('class','team-live-graph-arrowhead');marker.append(arrow);defs.append(marker);svg.append(defs);
      for(const edge of layout.edges){
        if(edge.points.length<2)continue;
        const path=document.createElementNS('http://www.w3.org/2000/svg','path');path.setAttribute('d',edge.points.map((point,index)=>(index?'L':'M')+point.x+','+point.y).join(' '));
        path.setAttribute('class','team-live-graph-edge '+this.edgeClass(edge,relations));path.dataset.from=edge.from;path.dataset.to=edge.to;path.setAttribute('marker-end','url(#team-live-arrow-'+this.generation+')');svg.append(path);
      }
      stage.append(svg);
      for(const node of nodes){
        const position=layout.nodes[node.id],card=this.renderTaskNode(node,relations,true);
        card.style.left=(position.x-position.width/2)+'px';card.style.top=(position.y-position.height/2)+'px';card.style.width=position.width+'px';card.style.height=position.height+'px';stage.append(card);
      }
      scroll.append(stage);section.append(scroll);
    }else{
      const list=this.node('div',undefined,'team-live-graph-list');
      for(const layer of teamLiveLayers(nodes)){
        const row=this.node('div',undefined,'team-live-graph-layer');for(const node of layer)row.append(this.renderTaskNode(node,relations,false));list.append(row);
      }
      section.append(list);
    }
    return section;
  }
  edgeClass(edge,relations){
    const {selected,upstream,downstream,related}=relations;
    if(!this.effectiveSelection())return'related';
    if(!related.has(edge.from)||!related.has(edge.to))return'dimmed';
    if((upstream.has(edge.from)||selected.has(edge.from))&&(upstream.has(edge.to)||selected.has(edge.to))&&(upstream.has(edge.from)||upstream.has(edge.to)))return'upstream';
    if((downstream.has(edge.from)||selected.has(edge.from))&&(downstream.has(edge.to)||selected.has(edge.to))&&(downstream.has(edge.from)||downstream.has(edge.to)))return'downstream';
    return'selected';
  }
  nodeTasks(node){
    const tasks=teamLiveNodeTasks(node,this.detail);
    return node.runCount>1?tasks.slice(-1):tasks;
  }
  nodeTitle(node){
    const tasks=this.nodeTasks(node);
    return teamLiveTaskTitle(tasks.length===1?tasks[0]:null,node);
  }
  renderTaskNode(node,relations,positioned){
    const id=node.id,selected=relations.selected.has(id),classes=['team-live-task','team-graph-node'];
    if(positioned)classes.push('positioned');
    if(Array.isArray(node.memberIds)&&node.memberIds.length>1)classes.push('aggregate');
    if(this.effectiveSelection()){
      if(selected)classes.push('selected');
      else if(relations.upstream.has(id))classes.push('upstream');
      else if(relations.downstream.has(id))classes.push('downstream');
      else classes.push('dimmed');
    }
    const title=this.nodeTitle(node),card=this.button('',classes.join(' '),()=>this.select('task',id));
    card.dataset.liveTask=id;card.dataset.status=node.status||'';card.setAttribute('aria-pressed',String(this.pinned?.kind==='task'&&this.pinned.id===id));card.title=title;
    card.addEventListener('mouseenter',()=>this.preview(id,'hover'));card.addEventListener('mouseleave',()=>this.preview(null,'hover'));
    card.addEventListener('focus',()=>this.preview(id,'focus'));card.addEventListener('blur',()=>this.preview(null,'focus'));
    const memberIds=Array.isArray(node.memberIds)?node.memberIds:[node.memberId];
    const memberLabels=memberIds.filter(Boolean).map(memberId=>this.memberLabel(memberId));
    return appendTeamGraphNodeContent(card,node,{title,status:node.status,memberLabels});
  }
  renderSelection(){
    const box=this.node('section',undefined,'team-live-selection');box.id='team-live-selection';
    const selection=this.effectiveSelection();if(!selection){box.append(this.node('p',this.t('选择成员或任务，查看负责人、等待前置与下游解锁。'),'muted'));return box;}
    const nodes=this.nodes();
    if(selection.kind==='member'){
      const member=this.member(selection.id),state=teamMemberRunState(member,this.detail||this.run);
      box.append(this.node('h3',this.t('选中成员')+' · '+this.memberLabel(selection.id)),this.node('p',state.tasks.length+' '+this.t('个关联任务')+(state.task?' · '+teamLiveTaskTitle(state.task):'')));
      return box;
    }
    const node=nodes.find(row=>row.id===selection.id);if(!node){box.append(this.node('p',this.t('任务列表已更新，请重新选择。'),'muted'));return box;}
    const tasks=this.nodeTasks(node),byId=new Map(nodes.map(row=>[row.id,row]));
    box.append(this.node('h3',this.nodeTitle(node)));
    const owners=(node.memberIds||[]).map(id=>this.memberLabel(id)).join(' · ');
    box.append(this.node('p',this.t('负责人')+' · '+(owners||this.t('待分派'))+' · '+this.taskStatus(node.status)));
    const waiting=(node.dependsOn||[]).filter(id=>byId.get(id)?.status!=='completed');
    const waitingBox=this.node('div',undefined,'team-live-relation-line');waitingBox.append(this.node('span',waiting.length?this.t('等待：'):this.t('前置已满足')));
    for(const id of waiting){const dependency=byId.get(id);if(dependency)waitingBox.append(this.button(this.nodeTitle(dependency),'plain team-live-related-task',()=>this.select('task',id)));else waitingBox.append(this.node('span',id));}
    box.append(waitingBox);
    const downstream=nodes.filter(row=>(row.dependsOn||[]).includes(node.id)),downstreamBox=this.node('div',undefined,'team-live-relation-line');
    downstreamBox.append(this.node('span',downstream.length?this.t('完成后解锁：'):this.t('没有下游任务')));
    for(const row of downstream)downstreamBox.append(this.button(this.nodeTitle(row),'plain team-live-related-task',()=>this.select('task',row.id)));
    box.append(downstreamBox);
    const actions=this.node('div',undefined,'team-live-actions');
    for(const task of tasks){
      const member=this.member(task.memberId),sessionId=typeof teamTaskSessionId==='function'?teamTaskSessionId(task,this.detail,member):member?.sessionId;
      const suffix=tasks.length>1?' · '+this.memberLabel(task.memberId)+' · '+teamLiveTaskTitle(task):'';
      const open=this.button(this.t('打开会话')+suffix,'plain',()=>this.openTaskSession(task));open.disabled=!sessionId;actions.append(open);
      actions.append(this.button(this.t('定位任务')+suffix,'plain',()=>this.openRun?.(this.team.id,this.run.id,{taskId:task.id})));
    }
    if(actions.children?.length||actions.childNodes?.length)box.append(actions);
    return box;
  }
}

if(typeof module!=='undefined'&&module.exports)module.exports={TeamLivePanelController,teamMemberRunState,teamGraphRelations,teamSelectionAfterRefresh,teamLiveFallbackNodes,teamLiveLayers,teamLiveNodeTasks};
