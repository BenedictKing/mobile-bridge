'use strict';

class TeamChatController{
  constructor({root,request,openRun}){
    Object.assign(this,{root,request,openRun});
    this.teams=[];this.current=null;this.run=null;this.team=null;
    this.pending=false;this.queued=false;this.lastFetch=0;this.generation=0;this.compact=false;
  }
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  button(text,fn){const node=this.node('button',text);node.type='button';node.onclick=fn;return node;}
  setTeams(teams){this.teams=Array.isArray(teams)?teams:[];}
  teamById(id){return this.teams.find(team=>team.id===id)||null;}
  teamForSession(sessionId){
    const matches=this.teams.filter(team=>(team.members||[]).some(member=>member.id===team.leadId&&member.sessionId===sessionId));
    return matches.length===1?matches[0]:null;
  }
  open(team){
    clearTimeout(this.timer);
    this.current=team||null;this.team=team||null;this.run=null;this.compact=false;this.generation++;
    this.render();if(this.current)this.refreshRun(true);
  }
  close(){this.open(null);}
  accepted(){this.refreshRun(true);}
  schedule(){
    clearTimeout(this.timer);if(!this.current)return;
    if(this.run&&TEAM_TERMINAL_RUN_STATUSES.has(this.run.status))return;
    this.timer=setTimeout(()=>this.refreshRun(true),4000);this.timer.unref?.();
  }
  async refreshRun(force=false){
    const team=this.current;
    if(!team)return;
    const now=Date.now();
    if(!force&&now-this.lastFetch<4000)return;
    if(this.pending){this.queued=true;return;}
    this.pending=true;this.queued=false;this.lastFetch=now;
    const generation=this.generation;
    try{
      const listed=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/runs');
      const rows=listed.runs||[],latest=rows[rows.length-1];
      if(!latest){if(generation===this.generation){this.run=null;this.render();}return;}
      const detail=await this.request('/api/teams/'+encodeURIComponent(team.id)+'/runs/'+encodeURIComponent(latest.id));
      if(generation===this.generation&&this.current?.id===team.id){this.team=team;this.run=detail;this.render();}
    }catch(error){
      if(generation===this.generation&&this.current?.id===team.id){this.run=null;this.render();}
    }finally{
      this.pending=false;
      if(this.queued&&this.current)this.refreshRun(true);else this.schedule();
    }
  }
  render(){
    const root=this.root,team=this.team,run=this.run;
    root.replaceChildren();
    if(!this.current||!team||!run){root.hidden=true;return;}
    root.hidden=false;
    const tasks=Array.isArray(run.tasks)?run.tasks:[],members=new Map((team.members||[]).map(member=>[member.id,member]));
    const running=tasks.filter(task=>['dispatching','running','ready'].includes(task.status)).length;
    const completed=tasks.filter(task=>task.status==='completed').length;
    const card=this.node('section',undefined,'team-workflow-card'+(this.compact?' compact':''));
    card.setAttribute('aria-label',t_safe('Workflow 运行摘要'));
    const head=this.node('div',undefined,'team-workflow-head');
    head.append(this.node('strong','Workflow · '+teamRunStatusLabel(run.status)));
    head.append(this.node('span',completed+'/'+tasks.length+' '+t_safe('任务')+(running?' · '+running+' '+t_safe('运行中'):''),'team-workflow-meta'));
    const toggle=this.button(this.compact?t_safe('展开成员'):t_safe('收起成员'),()=>{this.compact=!this.compact;this.render();});
    toggle.setAttribute('aria-expanded',String(!this.compact));
    head.append(toggle);
    head.append(this.button(t_safe('详情'),()=>this.openRun&&this.openRun(team.id,run.id)));
    card.append(head);
    const scroller=this.node('div',undefined,'team-workflow-scroll');
    scroller.tabIndex=0;scroller.setAttribute('aria-label',t_safe('阶段轨道，可横向滚动'));
    const stations=this.node('div',undefined,'team-workflow-stations');
    for(const task of tasks){
      const member=members.get(task.memberId)||{},status=task.status||'planned';
      const station=this.node('div',undefined,'team-workflow-station '+status);
      station.append(this.node('span',undefined,'team-workflow-lamp'));
      station.append(this.node('span',task.id==='lead'?t_safe('负责人'):(task.id||t_safe('任务')),'team-workflow-name'));
      station.append(this.node('span',teamTaskStatusLabel(status),'team-workflow-state'));
      const pills=this.node('div',undefined,'team-workflow-pills');
      const label=(member.name||member.id||task.memberId||t_safe('成员'))+(member.agentKind?' · '+BridgeI18n.agentLabel(member.agentKind):'');
      const pill=this.node('span',label,'team-workflow-pill');
      pill.title=task.instructionsHead||task.prompt||task.id||'';
      pills.append(pill);station.append(pills);stations.append(station);
    }
    scroller.append(stations);card.append(scroller);
    const results=tasks.filter(task=>task.result).length;
    if(results||run.verify){
      const artifacts=this.node('div',undefined,'team-workflow-artifacts');
      if(results)artifacts.append(this.node('span',results+' '+t_safe('份任务结果'),'team-workflow-artifact'));
      if(run.verify)artifacts.append(this.node('span',t_safe('验证退出码 ')+run.verify.exitCode,'team-workflow-artifact'));
      card.append(artifacts);
    }
    root.append(card);
  }
}
if(typeof module!=='undefined')module.exports={TeamChatController};
