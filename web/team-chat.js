'use strict';

class TeamChatController{
  constructor({root,request,openRun,onChange}){
    Object.assign(this,{root,request,openRun,onChange});
    this.teams=[];this.current=null;this.run=null;this.team=null;
    this.pending=false;this.queued=false;this.lastFetch=0;this.generation=0;this.compact=false;
  }
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  button(text,fn){const node=this.node('button',text);node.type='button';node.onclick=fn;return node;}
  setTeams(teams){this.teams=Array.isArray(teams)?teams:[];}
  teamById(id){return this.teams.find(team=>team.id===id)||null;}
  teamForProject(project){
    const key=typeof project==='string'?project:project.key,cwd=typeof project==='string'?'':project.cwd;
    return this.teams.find(team=>team.defaultForProject&&(team.project===key||(cwd&&team.projectCwd===cwd)))
      ||this.teams.find(team=>team.project===key||(cwd&&team.projectCwd===cwd))||null;
  }
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
  openSettings(){if(this.current)new TeamSettingsDialog({request:this.request,onChange:team=>{this.onChange&&this.onChange(team);}}).open(this.current);}
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
class TeamSettingsDialog{
  constructor({request,onChange}){this.request=request;this.onChange=onChange;this._dialog=null;this.team=null;this.agents=[];this.busy=false;this.error='';}
  node(tag,text,cls){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node;}
  button(text,fn){const node=this.node('button',text);node.type='button';node.onclick=fn;return node;}
  dialog(){
    if(this._dialog)return this._dialog;
    const dialog=document.createElement('dialog');dialog.className='picker team-settings-dialog';
    const head=this.node('div','', 'picker-head'),title=this.node('h2',t_safe('团队设置')),close=this.button('×',()=>dialog.close());
    close.className='icon-button';close.setAttribute('aria-label',t_safe('关闭'));head.append(title,close);
    this.body=this.node('div',undefined,'picker-body');dialog.append(head,this.body);document.body.append(dialog);this._dialog=dialog;return dialog;
  }
  async open(team){
    const dialog=this.dialog();this.team=team;this.error='';this.render();dialog.showModal();
    try{
      const detail=await this.request('/api/teams/'+encodeURIComponent(team.id));this.team=detail;
      if(detail.project){
        try{const value=await this.request('/api/projects/'+encodeURIComponent(detail.project)+'/agents');this.agents=value.agents||[];}catch{this.agents=[];}
      }
    }catch(error){this.error=error.message;}
    this.render();
  }
  render(){
    if(!this.body)return;
    this.body.replaceChildren();
    if(!this.team){this.body.append(this.node('p',t_safe('正在读取团队…'),'muted'));return;}
    const team=this.team,members=team.members||[];
    const leadSection=this.node('section',undefined,'team-settings-section');
    leadSection.append(this.node('h3',t_safe('主控')));
    for(const member of members){
      const label=this.node('label',undefined,'team-settings-lead');
      const radio=document.createElement('input');radio.type='radio';radio.name='team-lead';radio.checked=member.id===team.leadId;radio.disabled=this.busy;
      radio.onchange=()=>this.changeLead(member.id);
      label.append(radio,this.node('span',(member.name||member.id)+' · '+BridgeI18n.agentLabel(member.agentKind)));
      leadSection.append(label);
    }
    this.body.append(leadSection);
    const memberSection=this.node('section',undefined,'team-settings-section');
    memberSection.append(this.node('h3',t_safe('成员')));
    for(const member of members){
      const row=this.node('div',undefined,'team-settings-member');
      row.append(this.node('span',(member.name||member.id)+' · '+BridgeI18n.agentLabel(member.agentKind)+(member.available===false?' · '+t_safe('不可用'):'')));
      if(member.id!==team.leadId)row.append(this.button(t_safe('移除'),()=>this.removeMember(member.id)));
      memberSection.append(row);
    }
    const candidates=this.agents.filter(agent=>!members.some(member=>member.agentKind===agent.agent));
    const select=document.createElement('select');
    for(const agent of candidates)select.append(new Option((agent.title||agent.agent)+' · '+agent.agent,agent.agent));
    const role=document.createElement('select');role.append(new Option(t_safe('成员'),'member'),new Option(t_safe('评审'),'reviewer'));
    const add=this.button(t_safe('＋ 添加 Agent'),()=>this.addMember(select.value,role.value));
    add.disabled=this.busy||!candidates.length;
    const addRow=this.node('div',undefined,'team-settings-add');addRow.append(select,role,add);memberSection.append(addRow);
    this.body.append(memberSection);
    if(this.error)this.body.append(this.node('p',this.error,'error'));
  }
  async changeLead(memberID){
    if(memberID===this.team.leadId)return;
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id),{method:'PATCH',body:{leadId:memberID,version:this.team.version}}));
  }
  async addMember(agent,role){
    if(!agent)return;
    const base='member-'+agent.replace(/[^A-Za-z0-9_-]/g,'-');let id=base,index=2;
    while((this.team.members||[]).some(member=>member.id===id))id=base+'-'+(index++);
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id)+'/members',{method:'POST',body:{id,agent,role,name:agent,version:this.team.version}}));
  }
  async removeMember(memberID){
    await this.mutate(()=>this.request('/api/teams/'+encodeURIComponent(this.team.id)+'/members/'+encodeURIComponent(memberID)+'?version='+this.team.version,{method:'DELETE'}));
  }
  async mutate(operation){
    if(this.busy)return;
    this.busy=true;this.error='';this.render();
    try{this.team=await operation();this.onChange&&this.onChange(this.team);}
    catch(error){this.error=error.message;}
    finally{this.busy=false;this.render();}
  }
}
if(typeof module!=='undefined')module.exports={TeamChatController,TeamSettingsDialog};
