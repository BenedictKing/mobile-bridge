'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
global.t_safe=text=>text;
global.teamRunStatusLabel=status=>status;
global.teamTaskStatusLabel=status=>status;
global.TEAM_TERMINAL_RUN_STATUSES=new Set(['completed','failed','cancelled','stopped','outcome_unknown']);
global.BridgeI18n={agentLabel:kind=>kind};
global.teamMemberAgentLabel=require('../web/team.js').teamMemberAgentLabel;
const {splitTeamActions,renderTeamActionsCard,renderTeamDeliveryEvidence,TeamChatController,TeamSettingsDialog,TeamLeadPicker,teamSubmissionFields,teamWorkflowStages}=require('../web/team-chat.js');

class FakeNode{
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.style={};this.hidden=false;this.className='';this.textContent='';this.dataset={};this.listeners={};}
  append(...nodes){for(const node of nodes)node._parent=this;this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=[...nodes];for(const node of nodes)node._parent=this;}
  remove(){const siblings=this._parent&&this._parent.children;if(siblings){const index=siblings.indexOf(this);if(index>=0)siblings.splice(index,1);}}
  insertBefore(node,reference){const index=this.children.indexOf(reference);if(index>=0)this.children.splice(index,0,node);else this.children.push(node);node._parent=this;}
  setAttribute(name,value){this.attributes[name]=value;}
  addEventListener(type,listener){(this.listeners[type]||(this.listeners[type]=[])).push(listener);}
  showModal(){this.open=true;}
  close(){this.open=false;for(const listener of this.listeners.close||[])listener();}
  get classList(){
    const node=this;
    return{add(name){if(!node.className.split(/\s+/).includes(name))node.className+=(node.className?' ':'')+name;},toggle(name,force){node.className=node.className.split(/\s+/).filter(Boolean).filter(part=>part!==name).concat(force===false?[]:[name]).join(' ');}};
  }
}
global.Option=class{constructor(text,value){this.textContent=text;this.value=value;this.className='';this.children=[];}};
global.document={createElement:tag=>new FakeNode(tag),createElementNS:(ns,tag)=>new FakeNode(tag),body:new FakeNode('body')};
global.BridgeIcons=require('../web/icons.js');
function find(node,cls){const rows=[];const visit=current=>{if(current.className.split(/\s+/).includes(cls))rows.push(current);for(const child of current.children)visit(child);};visit(node);return rows;}
function text(node){return node.textContent+node.children.map(text).join('');}
function flush(){return new Promise(resolve=>setImmediate(resolve));}

const teams=[{id:'team-1',name:'食材团队',leadId:'lead',members:[
  {id:'lead',sessionId:'sess-lead',agentKind:'kimi',name:'负责人'},
  {id:'dev',sessionId:'sess-dev',agentKind:'zcode',name:'前端'},
]}];

function setup(runDetail){
  const root=new FakeNode('div'),calls=[],opened=[];
  const controller=new TeamChatController({root,request:async path=>{
    calls.push(path);
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    return runDetail;
  },openRun:(teamId,runId)=>opened.push({teamId,runId})});
  return{root,calls,opened,controller};
}

test('team directory resolves only a unique lead session',()=>{
  const {controller}=setup({tasks:[]});
  controller.setTeams(teams);
  assert.equal(controller.teamById('team-1').name,'食材团队');
  assert.equal(controller.teamForSession('sess-lead').id,'team-1');
  assert.equal(controller.teamForSession('sess-dev'),null);
  controller.setTeams([{...teams[0],project:'acp:old',projectCwd:'/work',defaultForProject:true}]);
  assert.equal(controller.teamForProject({key:'codex:new',cwd:'/work'}).id,'team-1');
  controller.setTeams([...teams,{...teams[0],id:'team-2'}]);
  assert.equal(controller.teamForSession('sess-lead'),null);
});

test('workflow renders run tasks and supports member collapse and details',async()=>{
  const detail={id:'run-1',status:'running',tasks:[
    {id:'lead',memberId:'lead',status:'completed',prompt:'理解目标'},
    {id:'implement',memberId:'dev',status:'running',prompt:'核对首页'},
  ]};
  const{root,controller,opened}=setup(detail);
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(root.hidden,false);
  const stations=find(root,'team-workflow-station');
  assert.equal(stations.length,2);
  assert.equal(stations[1].dataset.status,'running');
  assert.equal(find(root,'team-workflow-lamp').length,0);
  assert.equal(find(root,'team-workflow-node-head').length,2);
  assert.equal(find(root,'team-badge').some(node=>node.className.includes('b-running')),true);
  assert.match(text(find(root,'team-workflow-member')[1]),/前端 · zcode/);
  assert.equal(find(root,'team-workflow-member-label')[1].className.includes('team-graph-kind'),true);
  const toggle=find(root,'team-workflow-head')[0].children[2];
  toggle.onclick();
  assert.equal(find(root,'team-workflow-head')[0].children[2].attributes['aria-expanded'],'false');
  assert.equal(find(root,'team-workflow-card')[0].className.includes('compact'),true);
  const details=find(root,'team-workflow-head')[0].children[3];
  details.onclick();
  assert.deepEqual(opened,[{teamId:'team-1',runId:'run-1'}]);
  controller.close();assert.equal(root.hidden,true);
});

test('team chat prefers an unfinished run over a newer completed run',async()=>{
  const root=new FakeNode('div'),calls=[];
  const detail={id:'run-waiting',status:'waiting_input',tasks:[{id:'lead',memberId:'lead',status:'outcome_unknown',prompt:'先实现'}]};
  const controller=new TeamChatController({root,request:async path=>{
    calls.push(path);
    if(path.endsWith('/runs'))return{runs:[{id:'run-waiting',status:'waiting_input'},{id:'run-newer',status:'completed'}]};
    return detail;
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(controller.run.id,'run-waiting');
  assert.ok(calls.includes('/api/teams/team-1/runs/run-waiting'));
  assert.equal(calls.includes('/api/teams/team-1/runs/run-newer'),false);
  controller.close();
});

test('team settings lists enabled agents without project history and adds with optimistic version',async()=>{
  const team={...teams[0],project:'acp:project',version:2};
  let added=null,changed=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/projects')return{agents:[
      {kind:'codex',label:'Codex'},{kind:'kimi',label:'Kimi'},
      {kind:'zcode',label:'ZCode'},{kind:'claude',label:'Claude'},
    ]};
    if(path==='/api/teams/team-1/members'){
      added=options.body;
      return{...team,members:[...team.members,{id:added.id,sessionId:'sess-new',agentKind:'claude',name:'claude',role:added.role}]};
    }
    throw new Error('unexpected request '+path);
  },onChange:updated=>{changed=updated;}});
  await settings.open(team);
  assert.equal(settings._dialog.open,true);
  assert.equal(find(settings.body,'team-role-select').length,2);
  const choices=find(settings.body,'team-add-agent')[0].children;
  assert.deepEqual(choices.map(option=>option.value),['kimi','zcode','claude']);
  assert.deepEqual(choices.map(option=>option.textContent),['Kimi','ZCode','Claude']);
  await settings.addMember('claude','reviewer');
  assert.equal(added.id,'member-claude');
  assert.equal(added.role,'reviewer');
  assert.equal(added.version,2);
  assert.equal(changed.members.length,3);
});

test('team settings owns lead compaction and reset actions',async()=>{
  const team={...teams[0],version:2};
  const actions=[];
  const settings=new TeamSettingsDialog({request:async path=>{
    if(path==='/api/teams/team-1')return team;
    throw new Error('unexpected request '+path);
  },onCompactLead:(current,close)=>{actions.push(['compact',current.id]);close();},onResetLead:(current,close)=>{actions.push(['reset',current.id]);close();}});
  await settings.open(team);
  assert.deepEqual(find(settings.body,'team-context-compact').map(button=>button.textContent),['压缩主控上下文']);
  assert.deepEqual(find(settings.body,'team-context-reset').map(button=>button.textContent),['重置主控会话']);
  find(settings.body,'team-context-compact')[0].onclick();
  assert.equal(settings._dialog.open,false);
  await settings.open(team);
  find(settings.body,'team-context-reset')[0].onclick();
  assert.deepEqual(actions,[['compact','team-1'],['reset','team-1']]);
  assert.equal(settings._dialog.open,false);
});

test('team settings renders capability tags as capsules removable before save',async()=>{
  const team={...teams[0],version:2,members:teams[0].members.map(member=>({...member,tags:member.id==='dev'?['前端','React']:[]}))};
  const patches=[];
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/teams/team-1/members/dev'&&options.method==='PATCH'){
      patches.push(options.body);
      return{...team,version:3,members:team.members.map(member=>member.id==='dev'?{...member,tags:options.body.tags}:member)};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const chipTexts=()=>find(settings.body,'team-tag-chip').map(chip=>chip.children[0].textContent);
  assert.deepEqual(chipTexts(),['前端','React']);
  assert.deepEqual(find(settings.body,'team-member-save-tags').map(button=>button.disabled),[true,true]);
  // 点 × 立即从界面摘下胶囊，但未保存不落库。
  find(settings.body,'team-tag-chip-remove')[0].onclick();
  assert.equal(patches.length,0);
  assert.deepEqual(chipTexts(),['React']);
  assert.equal(find(settings.body,'team-member-save-tags')[1].disabled,false);
  find(settings.body,'team-member-save-tags')[1].onclick();await flush();
  assert.deepEqual(patches,[{tags:['React'],version:2}]);
  assert.deepEqual(settings.team.members[1].tags,['React']);
  assert.deepEqual(find(settings.body,'team-member-save-tags').map(button=>button.disabled),[true,true]);
});

test('team settings offers capability presets and toggles read-only advisor',async()=>{
  const team={...teams[0],version:2,members:teams[0].members.map(member=>({...member,tags:member.id==='dev'?['代码审查']:[]}))};
  const patches=[];
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/projects')return{agents:[]};
    if(path==='/api/teams/team-1/members/dev'){patches.push(options.body);return{...team,version:3,members:team.members.map(member=>member.id==='dev'?{...member,...options.body}:member)};}
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const presets=find(settings.body,'team-tag-presets');
  assert.equal(presets.length,2);
  assert(!presets[1].children.some(chip=>chip.textContent.includes('代码审查')),'已有的预设不再重复提供');
  presets[1].children.find(chip=>chip.textContent.includes('后端/算法')).onclick();
  assert.deepEqual(find(settings.body,'team-tag-chip').map(chip=>chip.children[0].textContent),['代码审查','后端/算法']);
  assert.equal(find(settings.body,'team-member-save-tags')[1].disabled,false);
  const toggles=find(settings.body,'team-member-advisor');
  assert.equal(toggles.length,1,'主控不提供顾问开关');
  const checkbox=toggles[0].children[0];checkbox.checked=true;checkbox.onchange();await flush();
  assert.deepEqual(patches,[{role:'advisor',version:2}]);
  assert.match(find(settings.body,'team-member-role')[1].textContent,/只读顾问/);
});

test('team settings enables save only when typed or committed tags change the member',async()=>{
  const team={...teams[0],version:2,members:teams[0].members.map(member=>({...member,tags:member.id==='dev'?['前端']:[]}))};
  let patch=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/teams/team-1/members/dev'&&options.method==='PATCH'){
      patch=options.body;
      return{...team,version:3,members:team.members.map(member=>member.id==='dev'?{...member,tags:patch.tags}:member)};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const fields=find(settings.body,'team-member-tags'),save=find(settings.body,'team-member-save-tags');
  const emptyInput=fields[0].children.find(node=>node.tag==='input');
  const input=fields[1].children.find(node=>node.tag==='input');
  assert.deepEqual(save.map(button=>button.disabled),[true,true]);
  assert.match(emptyInput.placeholder,/前端/);
  assert.equal(input.placeholder,'');
  emptyInput.value='   ';emptyInput.oninput();
  assert.equal(save[0].disabled,true);
  input.value='前端';input.oninput();
  assert.equal(save[1].disabled,true);
  input.value='便宜';input.oninput();
  assert.equal(save[1].disabled,false);
  input.value='';input.oninput();
  assert.equal(save[1].disabled,true);
  input.value='便宜';input.oninput();
  save[1].onclick();await flush();
  assert.deepEqual(patch,{tags:['前端','便宜'],version:2});
  assert.deepEqual(find(settings.body,'team-member-save-tags').map(button=>button.disabled),[true,true]);
});

test('team settings accepts spaces, commas, enter, and quoted multiword tags',async()=>{
  const team={...teams[0],version:2,members:teams[0].members.map(member=>({...member,tags:member.id==='dev'?['前端']:[]}))};
  let patch=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/teams/team-1/members/dev'&&options.method==='PATCH'){
      patch=options.body;
      return{...team,version:3,members:team.members.map(member=>member.id==='dev'?{...member,tags:options.body.tags}:member)};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const chipTexts=()=>find(settings.body,'team-tag-chip').map(chip=>chip.children[0].textContent);
  const input=find(settings.body,'team-member-tags')[1].children.find(node=>node.tag==='input');
  // 回车提交：空段过滤、重复合并。
  input.value=' 前端，React, "代码 评审"，前端';
  input.oninput();
  assert.deepEqual(chipTexts(),['前端','React','代码 评审']);
  input.value='"Claude Sonnet"';
  input.oninput();
  assert.deepEqual(chipTexts(),['前端','React','代码 评审']);
  input.onkeydown({key:'Enter',preventDefault(){}});
  assert.deepEqual(chipTexts(),['前端','React','代码 评审','Claude Sonnet']);
  assert.equal(input.value,'');
  // 逗号即时提交完整标签，未完成的引号尾段保留到保存。
  input.value='Vue, "Solid Design"';
  input.oninput();
  assert.deepEqual(chipTexts(),['前端','React','代码 评审','Claude Sonnet','Vue']);
  assert.equal(input.value,'"Solid Design"');
  find(settings.body,'team-member-save-tags')[1].onclick();await flush();
  assert.deepEqual(patch,{tags:['前端','React','代码 评审','Claude Sonnet','Vue','Solid Design'],version:2});
  assert.deepEqual(settings.team.members[1].tags,['前端','React','代码 评审','Claude Sonnet','Vue','Solid Design']);
});

test('space-delimited tag entry creates capsules without splitting IME composition',async()=>{
  const team={...teams[0],version:2,members:teams[0].members.map(member=>({...member,tags:member.id==='dev'?['前端']:[]}))};
  let patch=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/teams/team-1/members/dev'&&options.method==='PATCH'){
      patch=options.body;
      return{...team,version:3,members:team.members.map(member=>member.id==='dev'?{...member,tags:patch.tags}:member)};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const chipTexts=()=>find(settings.body,'team-tag-chip').map(chip=>chip.children[0].textContent);
  const field=find(settings.body,'team-member-tags')[1];
  const input=field.children.find(node=>node.tag==='input');
  const save=find(settings.body,'team-member-save-tags')[1];
  input.value='便宜 稳定 灵活 ';input.oninput();
  assert.deepEqual(chipTexts(),['前端','便宜','稳定','灵活']);
  assert.equal(input.value,'');
  assert.equal(save.disabled,false);
  input.value='高 效 ';input.oninput({isComposing:true});
  assert.deepEqual(chipTexts(),['前端','便宜','稳定','灵活']);
  input.value='高效 ';input.oncompositionend();
  input.value='“代码 评审” "R&D, Ops" ';input.oninput();
  assert.deepEqual(chipTexts(),['前端','便宜','稳定','灵活','高效','代码 评审','R&D, Ops']);
  save.onclick();await flush();
  assert.deepEqual(patch,{tags:['前端','便宜','稳定','灵活','高效','代码 评审','R&D, Ops'],version:2});
});

test('team settings allows the lead to also be the default reviewer',async()=>{
  const team={...teams[0],project:'acp:project',version:2};
  let patch=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/projects')return{agents:[]};
    if(path==='/api/teams/team-1'&&options.method==='PATCH'){
      patch=options.body;return{...team,reviewerId:patch.reviewerId,version:3};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const reviewer=find(settings.body,'team-role-select')[1];
  assert.deepEqual(reviewer.children.map(option=>option.value),['','lead','dev']);
  reviewer.value='lead';reviewer.onchange();await flush();
  assert.deepEqual(patch,{reviewerId:'lead',version:2});
  assert.equal(settings.team.reviewerId,'lead');
  assert.equal(find(settings.body,'team-member-role')[0].textContent,'主控 · 评审');
});

test('team settings keeps member identity when selecting the default reviewer',async()=>{
  const team={...teams[0],project:'acp:project',version:2,reviewerId:'lead',members:teams[0].members.map(member=>({...member,role:member.id==='lead'?'lead':'member'}))};
  let patch=null;
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return team;
    if(path==='/api/projects')return{agents:[]};
    if(path==='/api/teams/team-1'&&options.method==='PATCH'){
      patch=options.body;return{...team,reviewerId:patch.reviewerId,version:3};
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(team);
  const reviewer=find(settings.body,'team-role-select')[1];
  reviewer.value='dev';reviewer.onchange();await flush();
  assert.deepEqual(patch,{reviewerId:'dev',version:2});
  assert.equal(settings.team.members[1].role,'member');
  assert.equal(find(settings.body,'team-member-role')[0].textContent,'主控');
  assert.equal(find(settings.body,'team-member-role')[1].textContent,'成员 · 评审');
});

test('team settings preserves member roles through repeated lead and reviewer switches',async()=>{
  const members=[
    {id:'lead',sessionId:'sess-lead',agentKind:'kimi',name:'kimi',role:'lead'},
    {id:'dev',sessionId:'sess-dev',agentKind:'zcode',name:'zcode',role:'member'},
    {id:'rev',sessionId:'sess-rev',agentKind:'mcode',name:'mcode',role:'member'},
  ];
  let current={id:'team-1',project:'acp:project',version:2,leadId:'lead',reviewerId:'',members};
  const patches=[];
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return current;
    if(path==='/api/projects')return{agents:[]};
    if(path==='/api/teams/team-1'&&options.method==='PATCH'){
      patches.push(options.body);current={...current,...options.body,version:current.version+1};return current;
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(current);
  const choose=async(index,value)=>{const select=find(settings.body,'team-role-select')[index];select.value=value;select.onchange();await flush();};
  const labels=()=>find(settings.body,'team-member-role').map(node=>node.textContent);
  await choose(1,'dev');
  assert.deepEqual(labels(),['主控','成员 · 评审','成员']);
  await choose(1,'rev');
  assert.deepEqual(labels(),['主控','成员','成员 · 评审']);
  await choose(1,'');
  assert.deepEqual(labels(),['主控','成员','成员']);
  await choose(0,'dev');
  assert.deepEqual(labels(),['成员','主控','成员']);
  await choose(1,'lead');
  assert.deepEqual(labels(),['成员 · 评审','主控','成员']);
  await choose(1,'dev');
  assert.deepEqual(labels(),['成员','主控 · 评审','成员']);
  await choose(0,'lead');
  assert.deepEqual(labels(),['主控','成员 · 评审','成员']);
  assert.deepEqual(patches,[
    {reviewerId:'dev',version:2},{reviewerId:'rev',version:3},{reviewerId:'',version:4},
    {leadId:'dev',version:5},{reviewerId:'lead',version:6},{reviewerId:'dev',version:7},{leadId:'lead',version:8},
  ]);
  assert.deepEqual(current.members,members);
});

test('team settings shows one effective reviewer and distinguishes same-kind members',async()=>{
  const members=[
    {id:'member-kimi',sessionId:'sess-lead',agentKind:'kimi',name:'kimi',role:'lead',enabled:true},
    {id:'member-zcode',sessionId:'sess-zcode',agentKind:'zcode',name:'zcode',role:'member',enabled:true},
    {id:'member-mcode',sessionId:'sess-mcode',agentKind:'mcode',name:'mcode',role:'reviewer',enabled:true},
    {id:'member-kimi-2',sessionId:'sess-kimi-2',agentKind:'kimi',name:'kimi-2',role:'member',enabled:true},
  ];
  let current={id:'team-1',project:'acp:project',version:12,leadId:'member-kimi',reviewerId:'member-zcode',members};
  const patches=[];
  const settings=new TeamSettingsDialog({request:async(path,options={})=>{
    if(path==='/api/teams/team-1'&&!options.method)return current;
    if(path==='/api/projects')return{agents:[]};
    if(path==='/api/teams/team-1'&&options.method==='PATCH'){
      patches.push(options.body);current={...current,...options.body,version:current.version+1};return current;
    }
    throw new Error('unexpected request '+path);
  }});
  await settings.open(current);
  const expectedOptions=['kimi','zcode','mcode','kimi · kimi-2'];
  for(const select of find(settings.body,'team-role-select'))assert.deepEqual(select.children.slice(-4).map(option=>option.textContent),expectedOptions);
  const labels=()=>find(settings.body,'team-member-role').map(node=>node.textContent);
  assert.deepEqual(labels(),['主控','成员 · 评审','成员','成员']);
  const choose=async value=>{const select=find(settings.body,'team-role-select')[1];select.value=value;select.onchange();await flush();};
  await choose('member-mcode');
  assert.deepEqual(labels(),['主控','成员','成员 · 评审','成员']);
  await choose('member-kimi-2');
  assert.deepEqual(labels(),['主控','成员','成员','成员 · 评审']);
  await choose('member-kimi');
  assert.deepEqual(labels(),['主控 · 评审','成员','成员','成员']);
  await choose('');
  assert.deepEqual(labels(),['主控','成员','成员 · 评审','成员']);
  assert.deepEqual(patches,[
    {reviewerId:'member-mcode',version:12},{reviewerId:'member-kimi-2',version:13},
    {reviewerId:'member-kimi',version:14},{reviewerId:'',version:15},
  ]);
  assert.deepEqual(current.members,members);
});

test('team without runs shows a ready empty state instead of a blank panel',async()=>{
  const root=new FakeNode('div');
  const controller=new TeamChatController({root,request:async()=>({runs:[]}),openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(root.hidden,false);
  assert.equal(find(root,'team-workflow-empty').length,1);
  assert.match(text(root),/团队已就绪/);
});

test('team /new keeps old runs hidden until a new workflow arrives',async()=>{
  const root=new FakeNode('div');
  let runs=[{id:'run-old',status:'completed'}];
  const controller=new TeamChatController({root,request:async path=>{
    if(path.endsWith('/runs'))return{runs};
    const latest=runs[runs.length-1];
    return{id:latest.id,status:latest.status,tasks:[]};
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.equal(controller.run.id,'run-old');
  controller.resetWorkflow('team-1',runs);
  assert.equal(controller.run,null);
  assert.equal(find(root,'team-workflow-empty').length,1);
  await controller.refreshRun(true);
  assert.equal(controller.run,null,'刷新不得重新拉回 /new 前的 Workflow');
  runs=[...runs,{id:'run-new',status:'running'}];
  await controller.refreshRun(true);
  assert.equal(controller.run.id,'run-new');
  controller.close();
});

test('first team lead picker returns the selected agent kind',async()=>{
  const calls=[];
  const picker=new TeamLeadPicker({request:async(path,options={})=>{
    calls.push({path,options});
    return{agents:[{agent:'kimi',title:'旧会话一'},{agent:'zcode',title:'旧会话二'}]};
  }});
  const selected=picker.open({key:'acp:project',name:'项目'});await flush();
  assert.equal(picker._dialog.open,true);
  assert.deepEqual(find(picker.body,'team-lead-choice').map(node=>node.textContent),['kimi','zcode']);
  find(picker.body,'team-lead-choice')[1].onclick();
  assert.equal(await selected,'zcode');
  assert.deepEqual(calls.map(call=>call.path),['/api/projects/acp%3Aproject/agents']);
  assert.equal(calls.filter(call=>call.options.method==='PUT').length,0);
});

test('cancelling first team lead picker returns null without creating a team',async()=>{
  const calls=[];
  const picker=new TeamLeadPicker({request:async(path,options={})=>{
    calls.push({path,options});
    return{agents:[{agent:'kimi',title:'Kimi'}]};
  }});
  const selected=picker.open({key:'acp:project',name:'项目'});await flush();
  find(picker.body,'plain')[0].onclick();
  assert.equal(await selected,null);
  assert.equal(calls.filter(call=>call.options.method==='PUT').length,0);
});

test('team submission fields always enter autoPlan orchestration without a mode selector',()=>{
  assert.deepEqual(teamSubmissionFields('team-1'),{teamMode:'team',teamId:'team-1',teamIssueRoutingVersion:'2'});
  const html=require('node:fs').readFileSync(require.resolve('../web/index.html'),'utf8');
  const app=require('node:fs').readFileSync(require.resolve('../web/app.js'),'utf8');
  assert.equal(html.includes('id="team-message-mode"'),false);
  assert.equal(app.includes("$('team-message-mode')"),false);
});

test('completed run shows evidence without accept or discard',async()=>{
  const root=new FakeNode('div'),calls=[],opened=[];
  const run={id:'run-1',status:'completed',version:3,tasks:[],delivery:{agent:{completed:1,total:1},verify:{configured:false},pendingQuestions:0}};
  const controller=new TeamChatController({root,request:async(path,options={})=>{
    calls.push({path,options});
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    return run;
  },openRun(teamId,runId){opened.push([teamId,runId]);}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  const actions=find(root,'team-delivery-actions');
  assert.equal(actions.length,1);
  assert.equal(actions[0].children.length,1,'只保留去 Run 页修订');
  assert.doesNotMatch(text(root),/接受成果|丢弃|等待你验收/);
  actions[0].children[0].onclick();
  assert.deepEqual(opened,[[teams[0].id,'run-1']]);
  assert.equal(calls.some(call=>call.path.endsWith('/delivery')),false);
});

test('delivery card renders the four evidence dimensions separately',async()=>{
  const run={id:'run-1',status:'completed',version:2,tasks:[],delivery:{
    agent:{completed:2,total:3},
    resultSummary:{text:'t1：接口已实现；t2：用例补齐',truncated:false},
    review:{taskId:'rev',verdict:'passed'},
    verify:{configured:true,status:'failed',exitCode:1},
    pendingQuestions:1
  }};
  const {root}=setup(run);
  const controller=new TeamChatController({root,request:async path=>{
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    return run;
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  const dims=find(root,'team-delivery-dim');
  assert.equal(dims.length,4);
  assert.match(dims[3].textContent,/待回答问题：1/);
  assert.match(dims[0].textContent,/Agent 任务：2\/3 正常结束/);
  assert.match(dims[1].textContent,/审查：已通过/);
  assert.match(dims[2].textContent,/验证：未通过/);
  assert.match(text(find(root,'team-delivery-summary')[0]),/接口已实现/);
  // 未参与评审 / 未配置闸门的降级展示。
  const bare={id:'run-2',status:'completed',version:1,tasks:[],delivery:{
    agent:{completed:1,total:1},
    resultSummary:{text:'',truncated:true},verify:{configured:false},pendingQuestions:0
  }};
  const root2=new FakeNode('div');
  const controller2=new TeamChatController({root:root2,request:async path=>{
    if(path.endsWith('/runs'))return{runs:[{id:'run-2'}]};
    return bare;
  },openRun(){}});
  controller2.setTeams(teams);controller2.open(teams[0]);await flush();
  const dims2=find(root2,'team-delivery-dim');
  assert.match(dims2[1].textContent,/审查：未参与/);
  assert.match(dims2[2].textContent,/验证：未配置/);
  assert.equal(find(root2,'team-delivery-summary').length,0);
});

test('workflow stages fan out parallel workers before the merge and final reduction',()=>{
  const stages=teamWorkflowStages([
    {id:'lead'},
    {id:'lead-d1-0',dependsOn:['lead']},
    {id:'lead-d1-1',dependsOn:['lead']},
    {id:'lead-d1-2',dependsOn:['lead-d1-0','lead-d1-1']},
    {id:'lead-auto-reduce',dependsOn:['lead','lead-d1-0','lead-d1-1','lead-d1-2']},
  ]);
  assert.deepEqual(stages.map(stage=>stage.map(task=>task.id)),[
    ['lead'],['lead-d1-0','lead-d1-1'],['lead-d1-2'],['lead-auto-reduce'],
  ]);
});

test('legacy delegate tasks without persisted dependencies get the same display topology',()=>{
  const stages=teamWorkflowStages([
    {id:'lead',memberId:'kimi'},
    {id:'lead-d1-0',memberId:'claude'},
    {id:'lead-d1-1',memberId:'dsh'},
    {id:'lead-d1-2',memberId:'kimi'},
    {id:'lead-auto-reduce',memberId:'kimi',dependsOn:['lead','lead-d1-0','lead-d1-1','lead-d1-2']},
  ]);
  assert.deepEqual(stages.map(stage=>stage.map(task=>task.id)),[
    ['lead'],['lead-d1-0','lead-d1-1'],['lead-d1-2'],['lead-auto-reduce'],
  ]);
});

test('workflow station title prefers the prompt over the internal id',()=>{
  const {teamWorkflowTaskTitle}=require('../web/team-chat.js');
  // 任务 id 是内部标识（lead-d1-0），轨道上要展示提示词首行。
  assert.equal(teamWorkflowTaskTitle({id:'lead-d1-0',instructionsHead:'## 重构登录接口\n补充测试'}),'重构登录接口');
  assert.equal(teamWorkflowTaskTitle({id:'lead-d1-1',prompt:'## 只读排查\n细节'}),'只读排查');
  // 提示词开头的绝对路径在窄列里只会占满两行，展示时去掉，正文交给 CSS 两行截断。
  assert.equal(teamWorkflowTaskTitle({id:'t',prompt:'在 /Users/petaflops/works/unsorted/mobile-bridge 只读排查 internal/httpd'}),
    '只读排查 internal/httpd');
  assert.equal(teamWorkflowTaskTitle({id:'t',prompt:'一'.repeat(60)}),'一'.repeat(60));
  // 超长英文标识（测试名、路径段）在窄列里会被折在词中间，显示时中截，原文仍在 title。
  assert.equal(teamWorkflowTaskTitle({id:'t',prompt:'排查 TestTeamIntegration2IdempotencyAndRestart 的失败'}),
    '排查 TestTeamIntegr…AndRestart 的失败');
  // 主控站点保留角色名，没有提示词时回落到任务 id。
  assert.equal(teamWorkflowTaskTitle({id:'lead',prompt:'汇总成果'}),'负责人');
  assert.equal(teamWorkflowTaskTitle({id:'custom'}),'custom');
  assert.equal(teamWorkflowTaskTitle({id:'branch',workflowId:'b',workflowTitle:'导出',prompt:'补齐导出验收'}),'[导出] 补齐导出验收');
  assert.equal(teamWorkflowTaskTitle({id:'lead-goal-check-1',goalFeedback:true}),'目标评估与续做');
});

test('workflow counts come from the tasks, not from the run status',()=>{
  const {teamWorkflowCounts}=require('../web/team-chat.js');
  const counts=teamWorkflowCounts([
    {status:'completed'},{status:'failed'},{status:'outcome_unknown'},
    {status:'running'},{status:'ready'},{status:'stopped'},{status:'planned'},
  ]);
  assert.deepEqual(counts,{total:7,completed:1,failed:2,running:2,stopped:1,pending:1});
  assert.deepEqual(teamWorkflowCounts([]),{total:0,completed:0,failed:0,running:0,stopped:0,pending:0});
});

test('workflow outcome never reports a run with failed tasks as completed',()=>{
  const {teamWorkflowOutcome}=require('../web/team-chat.js');
  const failed={total:3,failed:1,completed:2,running:0,stopped:0,pending:0};
  // Run 的 completed 只表示编排走完，任务里有失败就必须说「部分失败」。
  assert.deepEqual(teamWorkflowOutcome({status:'completed'},failed),{label:'部分失败',tone:'bad'});
  assert.deepEqual(teamWorkflowOutcome({status:'completed'},{total:3,failed:0,stopped:0}),
    {label:'执行结束，结果待确认',tone:'warn'});
  assert.deepEqual(teamWorkflowOutcome({status:'failed'},{total:3,failed:2}),{label:'已失败',tone:'bad'});
  assert.deepEqual(teamWorkflowOutcome({status:'stopped'},{total:3,failed:0}),{label:'已停止',tone:'idle'});
  assert.deepEqual(teamWorkflowOutcome({status:'collecting'},{total:3,failed:0}),{label:'collecting',tone:'warn'});
  assert.deepEqual(teamWorkflowOutcome({status:'outcome_unknown'},{total:1,failed:1}),{label:'结果未知',tone:'bad'});
});

test('workflow member label does not say the agent name twice',()=>{
  const {teamWorkflowMemberLabel}=require('../web/team-chat.js');
  assert.equal(teamWorkflowMemberLabel({name:'kimi',agentKind:'kimi'}),'kimi');
  assert.equal(teamWorkflowMemberLabel({name:'前端',agentKind:'zcode'}),'前端 · zcode');
  assert.equal(teamWorkflowMemberLabel({id:'member-claude',agentKind:'claude'}),'member-claude · claude');
  assert.equal(teamWorkflowMemberLabel({}, {memberId:'member-1'}),'member-1');
  assert.equal(teamWorkflowMemberLabel({agentKind:'kimi'}),'kimi');
});

test('lead action envelopes split into cards only when well formed',()=>{
  const envelope='<team-actions>\n{"actions":[{"type":"delegate","member":"dev","task":"实现接口","writeLock":"none"}]}\n</team-actions>';
  assert.deepEqual(splitTeamActions('计划如下\n'+envelope+'\n完毕').map(part=>part.type),['text','actions','text']);
  assert.deepEqual(splitTeamActions(envelope).map(part=>part.type),['actions']);
  // 围栏里的协议示例、半截流式块、非法 JSON 都保持原文。
  assert.deepEqual(splitTeamActions('```\n'+envelope+'\n```').map(part=>part.type),['text']);
  assert.deepEqual(splitTeamActions('<team-actions>{"actions":[').map(part=>part.type),['text']);
  assert.deepEqual(splitTeamActions('<team-actions>{bad}</team-actions>').map(part=>part.type),['text']);
  // 与网关 ExtractActionBlock 同一规则：正文提到标签名时取最内层开始标记；多个块整体不执行，保持原文。
  assert.deepEqual(splitTeamActions('I will emit a <team-actions> block now:\n'+envelope).map(part=>part.type),['text','actions']);
  assert.deepEqual(splitTeamActions(envelope+'\n再来一个\n'+envelope).map(part=>part.type),['text']);
  assert.deepEqual(splitTeamActions('</team-actions> '+envelope).map(part=>part.type),['text']);
  // 缩进代码块与引用里的围栏也算代码：协议示例不画成卡片。
  assert.deepEqual(splitTeamActions('示例：\n\n    '+envelope.replace(/\n/g,'\n    ')).map(part=>part.type),['text']);
  assert.deepEqual(splitTeamActions('> ```\n> '+envelope.replace(/\n/g,'\n> ')+'\n> ```').map(part=>part.type),['text']);
  // 调用方给出的代码行集合优先（markdown.js 用 token.map）。
  assert.deepEqual(splitTeamActions('前言\n'+envelope,()=>new Set([1])).map(part=>part.type),['text']);
  const card=renderTeamActionsCard(splitTeamActions(envelope)[0]);
  assert.equal(card.className,'team-actions-card');
  assert.match(text(card),/团队动作 · 1/);
  assert.match(text(card),/委派任务/);
  assert.match(text(card),/dev/);
  assert.match(text(card),/只读/);
  assert.match(text(card),/实现接口/);
  assert.match(text(card),/查看原文/);
  assert.match(text(card),/<team-actions>/,'原文保留在折叠区');
});

test('delivery evidence names isolated workspaces that are not merged',()=>{
  const box=new FakeNode('div'),make=(tag,value,cls)=>{const node=new FakeNode(tag);if(value!==undefined)node.textContent=value;if(cls)node.className=cls;return node;};
  renderTeamDeliveryEvidence(box,{agent:{completed:1,total:1},verify:{configured:false},pendingQuestions:0,
    workspaces:[{workspaceId:'ws-1',taskId:'iso'}],resultSummary:{text:'',truncated:false}},make);
  assert.match(text(box),/独立工作区：ws-1 \(iso\)/);
  assert.match(text(box),/不会自动合入主工作区/);
  assert.doesNotMatch(text(box),/待回答问题/,'没有待答问题时不显示该维度');
});

test('a failed refresh keeps the current workflow card',async()=>{
  const run={id:'run-1',status:'running',version:1,tasks:[{id:'lead',memberId:'lead',status:'running'}]};
  const root=new FakeNode('div');let fail=false;
  const controller=new TeamChatController({root,request:async path=>{
    if(fail)throw new Error('网关重启中');
    if(path.endsWith('/runs'))return{runs:[{id:'run-1'}]};
    return run;
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  assert.ok(find(root,'team-workflow-stations').length,'首次加载显示工作流卡');
  fail=true;await controller.refreshRun(true);
  assert.ok(find(root,'team-workflow-stations').length,'刷新失败不应退回首次引导');
  assert.match(text(root),/刷新失败：网关重启中/);
});

test('a recovered refresh clears the error even when the team has no runs yet',async()=>{
  const root=new FakeNode('div');let fail=false;
  const controller=new TeamChatController({root,request:async path=>{
    if(fail)throw new Error('网关重启中');
    return{runs:[]};
  },openRun(){}});
  controller.setTeams(teams);controller.open(teams[0]);await flush();
  fail=true;await controller.refreshRun(true);
  assert.match(text(root),/刷新失败：网关重启中/);
  fail=false;await controller.refreshRun(true);
  assert.doesNotMatch(text(root),/刷新失败/);
});

test('team settings mutations report a success confirmation',async()=>{
  const team={...teams[0],version:2};
  const notices=[];
  globalThis.BridgeFeedback={ok:text=>notices.push(text)};
  try{
    const settings=new TeamSettingsDialog({request:async()=>({...team,version:3})});
    await settings.open(team);
    await settings.mutate(async()=>({...team,version:4}));
    assert.deepEqual(notices,['团队设置已更新']);
    const failures=[];
    globalThis.BridgeFeedback={ok:text=>failures.push(text)};
    await settings.mutate(async()=>{throw Error('请求失败');});
    assert.deepEqual(failures,[],'失败不得报成功');
    assert.equal(settings.error,'请求失败');
  }finally{delete globalThis.BridgeFeedback;}
});

// ---- 输入队列面板（阶段 4，docs/team-demand-queue-plan.md §7）----

async function withQueueStorage(entries,run){
  const store=new Map(Object.entries(entries));
  globalThis.localStorage={getItem:key=>store.has(key)?store.get(key):null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)};
  try{return await run();}finally{delete globalThis.localStorage;}
}

test('submission fields opt into the persistent queue per team',async()=>{
  await withQueueStorage({'teamQueueMode:team-1':'1'},()=>{
    assert.deepEqual(teamSubmissionFields('team-1'),{teamMode:'team',teamId:'team-1',teamIssueRoutingVersion:'2',teamQueueVersion:'2'});
    assert.deepEqual(teamSubmissionFields('team-2'),{teamMode:'team',teamId:'team-2',teamIssueRoutingVersion:'2'});
  });
  assert.deepEqual(teamSubmissionFields('team-1'),{teamMode:'team',teamId:'team-1',teamIssueRoutingVersion:'2'});
});

test('queue panel lists inputs with reasons, work items and queued actions',async()=>{
  await withQueueStorage({'teamQueueMode:team-1':'1'},async()=>{
    const posts=[];
    const queueData={pendingCount:2,maxPendingInputs:64,inputs:[
      {inputId:'in-2',sequence:2,queueStatus:'queued',text:'第二条排队消息',revision:1,version:1},
      {inputId:'in-1',sequence:1,queueStatus:'waiting_input',text:'等待目标收敛的第一条',revision:2,version:3,
        priority:true,waitingReason:'团队有多个未结束的 Run，请先在 Run 详情页收敛目标（完成或取消）后重试',
        runIds:[],workItems:[]},
      {inputId:'in-0',sequence:0,queueStatus:'routed',text:'已路由的早期消息',revision:1,version:2,
        targetRunId:'run-9',runIds:['run-9'],workItems:[{id:'wi-in-0',taskIds:['lead']}]},
    ]};
    const root=new FakeNode('div');
    const controller=new TeamChatController({root,request:async(path,options)=>{
      if(path.endsWith('/inputs?limit=50'))return queueData;
      if(path.endsWith('/runs'))return{runs:[]};
      posts.push({path,method:options?.method,body:options?.body});
      return{};
    },openRun:(teamId,runId)=>posts.push({openRun:runId})});
    controller.setTeams(teams);controller.open(teams[0]);
    await flush();await flush();
    const panel=find(root,'team-queue-panel')[0];
    assert.ok(panel,'队列面板应渲染');
    const rows=find(root,'team-queue-row');
    assert.equal(rows.length,3);
    const labels=find(root,'team-queue-status').map(node=>node.textContent);
    assert.deepEqual(labels,['排队中','等待处理','已路由']);
    // 等待原因与路由提示分开说。
    assert.match(text(rows[1]),/多个未结束的 Run/);
    assert.match(text(rows[1]),/排队操作不可用/);
    // 已路由：目标 Run 可跳转，工作项计数可见，修订次数可见。
    assert.equal(find(rows[2],'team-queue-run').length,1);
    assert.match(text(rows[2]),/工作项 1/);
    assert.match(text(rows[1]),/第2次修订/);
    // 排队行有 立即发送/编辑/删除；waiting/routed 行没有。
    const queuedButtons=find(rows[0],'team-queue-actions')[0].children.map(node=>node.textContent);
    assert.deepEqual(queuedButtons,['立即发送','编辑','删除']);
    assert.equal(find(rows[1],'team-queue-actions').length,0);
    // 已是优先的排队输入不再显示「立即发送」。
    assert.equal(find(rows[1],'team-queue-priority').length,1);
    // 立即发送携带操作 UUID 与期望版本。
    const prioritize=find(rows[0],'team-queue-actions')[0].children[0];
    prioritize.onclick();
    await flush();await flush();
    const posted=posts.find(call=>call.path&&call.path.endsWith('/inputs/in-2/prioritize'));
    assert.ok(posted&&posted.method==='POST');
    assert.equal(posted.body.version,1);
    assert.ok(posted.body.id);
  });
});

test('queue mode off hides the standalone panel and queue requests',async()=>{
  const root=new FakeNode('div');
  const controller=new TeamChatController({root,request:async path=>{
    if(path.endsWith('/inputs?limit=50'))throw Error('不应拉取队列');
    if(path.endsWith('/runs'))return{runs:[]};
    return{};
  }});
  controller.setTeams(teams);controller.open(teams[0]);
  await flush();
  assert.equal(find(root,'team-queue-panel').length,0);
  assert.equal(find(root,'team-queue-toggle').length,0);
});

test('send queue selection owns the persistent mode without a second toggle',async()=>{
  await withQueueStorage({},async()=>{
    const root=new FakeNode('div');
    const controller=new TeamChatController({root,request:async path=>{
      if(path.endsWith('/inputs?limit=50'))return{inputs:[]};
      if(path.endsWith('/runs'))return{runs:[]};
      return{};
    }});
    controller.setTeams(teams);controller.open(teams[0]);await flush();
    controller.setQueueMode(true);await flush();
    assert.deepEqual(teamSubmissionFields('team-1'),{teamMode:'team',teamId:'team-1',teamIssueRoutingVersion:'2',teamQueueVersion:'2'});
    assert.equal(find(root,'team-queue-panel').length,1);
    assert.equal(find(root,'team-queue-toggle').length,0);
    controller.setQueueMode(false);
    assert.deepEqual(teamSubmissionFields('team-1'),{teamMode:'team',teamId:'team-1',teamIssueRoutingVersion:'2'});
    assert.equal(find(root,'team-queue-panel').length,0);
  });
  const app=require('node:fs').readFileSync(require.resolve('../web/app.js'),'utf8');
  assert.ok(app.includes("syncTeamQueueMode($('send-mode').value)"),'发送方式菜单应同步唯一排队开关');
  assert.ok(app.includes("teamQueueModeEnabled(currentTeam.id)?'queue':'send'"),'打开团队应恢复其发送方式');
});
