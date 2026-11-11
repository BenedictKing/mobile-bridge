'use strict';

const test=require('node:test');
const assert=require('node:assert/strict');
const {TeamCanvasController,teamCanvasAssociations,teamCanvasLinkedRecords,teamCanvasInputRunID,teamCanvasFitTransform,teamCanvasGroups,teamCanvasLayout,teamCanvasOwnerLine,teamCanvasDeliveryRows}=require('../web/team-canvas.js');

test('issue labels distinguish unconfirmed goals, resolution and execution problems',()=>{
  const c=Object.assign(Object.create(TeamCanvasController.prototype),{t:value=>value});
  assert.equal(c.statusLabel({status:'waiting',executionStatus:'completed',outcome:{label:'结果待确认'}}),'结果待确认');
  assert.equal(c.statusLabel({status:'resolved'}),'已解决');
  assert.equal(c.statusLabel({status:'waiting',executionStatus:'outcome_unknown',outcome:{label:'尚未评估'}}),'执行结果待核查');
});

function touchController(){
  const captured=new Set();
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
    touchPoints:new Map(),pinch:null,positions:new Map(),transform:{x:20,y:30,scale:.5},
    viewport:{getBoundingClientRect:()=>({left:10,top:20,width:390,height:600}),
      setPointerCapture:id=>captured.add(id),hasPointerCapture:id=>captured.has(id),releasePointerCapture:id=>captured.delete(id),classList:{add(){},remove(){}}},
    applyTransform(){},
  });
  const event=(id,x,y)=>({pointerId:id,pointerType:'touch',clientX:x,clientY:y,button:0,
    target:{closest:()=>null},preventDefault(){this.prevented=true;},stopPropagation(){this.stopped=true;}});
  return{controller,event,captured};
}

test('pinch zoom anchors the midpoint, supports two-finger pan and never moves a card',()=>{
  const{controller:c,event,captured}=touchController();
  c.startTouch(event(1,110,220));c.drag={type:'node',pointerId:1};
  const second=event(2,210,220);c.startTouch(second);
  assert.equal(c.drag,null);assert.equal(second.stopped,true);assert.deepEqual([...captured],[1,2]);
  c.moveTouch(event(1,60,220));c.moveTouch(event(2,260,220));
  assert.equal(c.transform.scale,1);
  assert.equal((150-c.transform.x)/c.transform.scale,260,'两指中点下的画布坐标不能漂移');
  assert.equal((200-c.transform.y)/c.transform.scale,340);
  c.moveTouch(event(1,90,260));c.moveTouch(event(2,290,260));
  assert.deepEqual(c.transform,{scale:1,x:-80,y:-100},'双指同向移动平移画布');
  assert.equal(c.positions.size,0);assert.equal(c.userInteracted,true);
  c.moveTouch(event(1,-1000,260));c.moveTouch(event(2,1000,260));assert.equal(c.transform.scale,1.6);
  c.moveTouch(event(1,999,260));assert.equal(c.transform.scale,.08);
  c.endTouch(event(1,999,260));c.suppressClickUntil=0;c.endTouch(event(2,1000,260));
  assert.equal(c.pinch,null);assert.equal(c.touchPoints.size,0);assert.ok(c.suppressClickUntil>Date.now(),'抬手不能误开事项或 Run');
});

test('single-touch pan still works and view changes clear captured pinch pointers',()=>{
  const{controller:c,event,captured}=touchController();
  const first=event(1,100,200);c.startTouch(first);c.startPan(first);
  const move=event(1,130,240);c.moveTouch(move);c.movePointer(move);
  assert.deepEqual(c.transform,{x:50,y:70,scale:.5});
  c.endTouch(move);c.endPointer(move);assert.equal(c.drag,null);
  c.startTouch(event(3,100,200));c.startTouch(event(4,200,200));
  c.resetTouch();assert.equal(c.touchPoints.size,0);assert.equal(captured.size,0);assert.equal(c.pinch,null);
  const detail=event(5,100,200);detail.target.closest=()=>({});c.startTouch(detail);
  assert.equal(c.touchPoints.size,0,'详情面板滚动不应变成画布缩放');
});

test('team view switching preserves canvas state and persists per team',()=>{
  const previousDocument=global.document,previousStorage=global.localStorage;
  const elements={};global.document={getElementById:id=>elements[id]||(elements[id]={setAttribute(name,value){this[name]=value;}})};
  const saved=[];global.localStorage={setItem:(...args)=>saved.push(args)};
  let boards=0,canvases=0,refreshes=0;
  const transform={x:20,y:45,scale:.8},positions=new Map([['m:input',{x:50,y:60}]]);
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{current:{id:'team-1'},root:{classList:{toggle(){}},append(){}},detail:{},viewport:{append(){}},boardRoot:{},summary:{},transform,positions,
    closeDetail(){},render(){canvases++;},refresh(){refreshes++;},boardPanel:{refreshAll(){boards++;}}});
  try{
    controller.setView('board');assert.equal(boards,0);assert.equal(controller.viewport.hidden,true);assert.equal(controller.boardRoot.hidden,false);
    assert.equal(elements['team-view-board']['aria-selected'],'true');assert.equal(elements['team-view-canvas'].tabIndex,-1);
    assert.equal(elements['team-canvas-fit'].hidden,true);assert.equal(canvases,1);
    controller.setView('canvas');assert.equal(canvases,2);assert.equal(refreshes,2);assert.equal(controller.boardRoot.hidden,true);
    assert.equal(controller.transform,transform);assert.equal(controller.positions,positions);
    assert.deepEqual(saved,[['team-view:team-1','board'],['team-view:team-1','canvas']]);
  }finally{global.document=previousDocument;global.localStorage=previousStorage;}
});

test('standalone message deletion uses its version and refreshes only after success',async()=>{
  const previousWindow=global.window;global.window={confirm:()=>true};
  try{
    const calls=[],button={disabled:false};
    const controller=Object.assign(Object.create(TeamCanvasController.prototype),{current:{id:'team'},error:{},
      request:async(...args)=>{calls.push(args);assert.equal(button.disabled,true);},
      refresh:async()=>calls.push('refresh'),notify:message=>calls.push(message)});
    await controller.deleteInput({inputId:'input',text:'继续',version:7},button);
    assert.deepEqual(calls,[['/api/teams/team/inputs/input',{method:'DELETE',body:{version:7}}],'消息已删除','refresh']);
    assert.equal(button.disabled,false);
    calls.length=0;global.window.confirm=()=>false;
    await controller.deleteInput({inputId:'input'},button);assert.equal(calls.length,0);
    global.window.confirm=()=>true;controller.request=async()=>{throw Error('仍有未结束的 Run');};
    await controller.deleteInput({inputId:'input'},button);
    assert.equal(controller.error.textContent,'仍有未结束的 Run');assert.equal(calls.length,0);assert.equal(button.disabled,false);
  }finally{global.window=previousWindow;}
});

test('canvas uses persisted issue links and ignores stale revisions',()=>{
  const associations=teamCanvasAssociations(
    [
      {inputId:'input-1',revision:2,text:'修复团队导航'},
      {inputId:'input-2',revision:1,text:'保留筛选'},
    ],
    [
      {id:'issue-nav',inputs:[{inputId:'input-1',revision:2,sourceText:'修复团队导航'}]},
      {id:'issue-crosscut',inputs:[{inputId:'input-1',revision:2,sourceText:'修复团队导航'}]},
      {id:'issue-stale',inputs:[{inputId:'input-1',revision:1,sourceText:'旧版导航'}]},
      {id:'issue-filter',inputs:[{inputId:'input-2',revision:1,sourceText:'保留筛选'}]},
    ],
  );
  assert.deepEqual(associations,{'input-1':['issue-nav','issue-crosscut'],'input-2':['issue-filter']});
});

test('canvas does not guess an issue from matching run or message text',()=>{
  const associations=teamCanvasAssociations(
    [{inputId:'input-1',revision:1,text:'继续完善团队页面'}],
    [],
  );
  assert.deepEqual(associations,{});
});

test('message without an Issue opens its latest linked Run',()=>{
  const input={runIds:['older-run','latest-run']};
  assert.equal(teamCanvasInputRunID(input,[]),'latest-run');
  assert.equal(teamCanvasInputRunID(input,['issue-1']),'');
  assert.equal(teamCanvasInputRunID({runIds:[]},[]),'');
});

test('closed issue retains the last executor',()=>{
  assert.equal(teamCanvasOwnerLine({lastOwner:{name:'Kimi'}}),'最后执行人 · Kimi');
  assert.equal(teamCanvasOwnerLine({lastOwner:{name:'member-kimi',memberId:'member-kimi',agentKind:'kimi'}},value=>value,kind=>kind.toUpperCase()),'最后执行人 · KIMI');
  assert.equal(teamCanvasOwnerLine({lastOwner:{name:'member-kimi',memberId:'member-kimi'}},value=>value,kind=>({kimi:'Kimi'}[kind]||kind)),'最后执行人 · Kimi');
  assert.equal(teamCanvasOwnerLine({currentOwner:{name:'ZCode'},lastOwner:{name:'Kimi'}}),'当前执行人 · ZCode');
  assert.equal(teamCanvasOwnerLine({}), '');
});

test('issue detail shows bounded delivery, review and verification evidence',()=>{
  const rows=teamCanvasDeliveryRows({delivery:{
    resultSummary:{text:'导航已完成，验证通过。',truncated:true},
    review:{verdict:'passed',reviewIndependence:false},
    verify:{configured:true,status:'passed',exitCode:0},
    pendingQuestions:1,
    workspaces:[{workspaceId:'ws-1'}],
  }});
  assert.deepEqual(rows,[
    '导航已完成，验证通过。 · 内容已截断',
    '评审结果 · passed · 非独立审查',
    '验证结果 · passed · 退出码 0',
    '待回答问题 · 1',
    '工作区 · ws-1',
  ]);
});

test('canvas wires reference rendered bubble nodes and omit missing source records',()=>{
  const one={input:{inputId:'input-1'},node:{style:{left:'0px'}}};
  const two={input:{inputId:'input-2'},node:{style:{left:'1px'}}};
  const records=teamCanvasLinkedRecords([one,two],[
    {issue:{id:'issue-1'},bubbles:[{input:{inputId:'input-2'}},{input:{inputId:'input-2'}},{input:{inputId:'missing'}}]},
  ]);
  assert.deepEqual(records[0].bubbles,[two]);
  assert.equal(records[0].bubbles[0].node.style.left,'1px');
});

const bubbleRecord=(id,height=96)=>({input:{inputId:id},node:{style:{},offsetHeight:height}});
const issueRecord=(id,ids,height=220)=>({issue:{id},bubbles:ids.map(inputId=>({input:{inputId}})),node:{style:{},offsetHeight:height}});
function assertLayoutBounds(plan){
  const boxes=plan.positions.map(p=>({...p,width:parseFloat(p.record.node.style.width),height:p.record.node.offsetHeight}));
  assert.equal(new Set(boxes.map(p=>p.key)).size,boxes.length,'节点不可重复');
  for(let index=0;index<boxes.length;index++){
    const a=boxes[index];
    assert.ok(a.x>=0&&a.y>=0&&a.x+a.width<=plan.width+.01&&a.y+a.height<=plan.height+.01,'节点应在画布范围内');
    for(const b of boxes.slice(index+1))assert.ok(a.x+a.width<=b.x+.01||b.x+b.width<=a.x+.01||a.y+a.height<=b.y+.01||b.y+b.height<=a.y+.01,'节点不可重叠：'+a.key+' / '+b.key);
  }
}

test('joint layout aligns every message with its taller issue instead of accumulating drift',()=>{
  const bubbles=Array.from({length:20},(_,i)=>bubbleRecord('input-'+i));
  const issues=bubbles.map((b,i)=>issueRecord('issue-'+i,[b.input.inputId],180+i*8)).reverse();
  const plan=teamCanvasLayout(bubbles,issues,{width:940});
  for(const bubble of bubbles){
    const left=plan.positions.find(p=>p.record===bubble),right=plan.positions.find(p=>p.key==='i:issue-'+bubbles.indexOf(bubble));
    assert.equal(left.y+bubble.node.offsetHeight/2,right.y+right.record.node.offsetHeight/2);
  }
  assertLayoutBounds(plan);
});

test('connected messages and issues form one group without duplicating shared nodes',()=>{
  const bubbles=['one','two','three','four'].map(id=>bubbleRecord(id));
  const issues=[issueRecord('shared',['one','three','three']),issueRecord('split',['three','four']),issueRecord('orphan',['missing'])];
  const groups=teamCanvasGroups(bubbles,issues);
  assert.deepEqual(groups.map(g=>g.bubbles.map(b=>b.input.inputId)),[['one','three','four'],['two'],[]]);
  assert.deepEqual(groups.map(g=>g.issues.map(i=>i.issue.id)),[['shared','split'],[],['orphan']]);
  const plan=teamCanvasLayout(bubbles,issues,{width:1480});
  assert.equal(plan.positions.length,7);
  assertLayoutBounds(plan);
});

test('wide layouts place independent pairs side by side and fan-out issues in multiple columns',()=>{
  const bubbles=Array.from({length:6},(_,i)=>bubbleRecord('input-'+i));
  const issues=bubbles.map((b,i)=>issueRecord('issue-'+i,[b.input.inputId]));
  const narrow=teamCanvasLayout(bubbles,issues,{width:940}),wide=teamCanvasLayout(bubbles,issues,{width:1480});
  assert.ok(wide.height<narrow.height*.65,'宽屏应减少纵向堆叠');
  assert.ok(wide.positions.some(p=>p.x>wide.width/2),'应使用右半屏');
  assertLayoutBounds(wide);
  const fanOut=Array.from({length:7},(_,i)=>issueRecord('fan-'+i,['input-0']));
  const grid=teamCanvasLayout([bubbles[0]],fanOut,{width:2100});
  const cards=grid.positions.filter(p=>p.key.startsWith('i:'));
  assert.equal(new Set(cards.map(p=>p.x)).size,3);
  assert.ok(cards.every(p=>p.grid&&parseFloat(p.record.node.style.width)>=300));
  assertLayoutBounds(grid);
});

test('mobile groups keep messages beside their own context in reading order',()=>{
  const bubbles=[bubbleRecord('one'),bubbleRecord('two')],issues=[issueRecord('first',['one']),issueRecord('second',['two'])];
  const plan=teamCanvasLayout(bubbles,issues,{width:390,mobile:true});
  assert.deepEqual(plan.positions.map(p=>p.key),['m:one','i:first','m:two','i:second']);
  assert.ok(plan.positions.every(p=>p.x===16));
  assertLayoutBounds(plan);
});

test('mobile controller fits full-width cards without shrinking or losing drag overrides',()=>{
  const previousWindow=global.window;
  global.window={matchMedia:()=>({matches:true})};
  try{
    const bubbles=[bubbleRecord('one')],issues=[issueRecord('first',['one'])];
    for(const record of [...bubbles,...issues])Object.defineProperty(record.node,'offsetWidth',{get(){return parseFloat(this.style.width);}});
    const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
      viewport:{clientWidth:390,getBoundingClientRect:()=>({width:390,height:500})},positions:new Map(),
      world:{style:{}},wires:{setAttribute(){}},zoomLabel:{},
    });
    controller.layout(bubbles,issues);
    controller.fitAll(false,true);
    assert.equal(controller.canvasWidth,390);
    assert.equal(controller.transform.scale,1,'默认按可读宽度显示');
    controller.positions.set('m:one',{x:12,y:40});
    controller.layout(bubbles,issues);
    assert.equal(bubbles[0].node._canvasPosition.x,12);
    assert.equal(bubbles[0].node._canvasPosition.y,40);
    controller.fitAll();
    assert.ok(controller.transform.scale<1,'仍可切换完整总览');
  }finally{global.window=previousWindow;}
});

test('fit all respects tall canvas bounds and centers the complete layout',()=>{
  const viewport={width:1000,height:600},canvas={width:940,height:1600};
  const fit=teamCanvasFitTransform(viewport.width,viewport.height,canvas.width,canvas.height);
  const right=fit.x+canvas.width*fit.scale,bottom=fit.y+canvas.height*fit.scale;
  assert.ok(fit.scale<.48);
  assert.ok(fit.x>=0&&right<=viewport.width);
  assert.ok(fit.y>=0&&bottom<=viewport.height);
  assert.equal(teamCanvasFitTransform(0,600,940,1600),null);
  assert.equal(teamCanvasFitTransform(40,600,940,1600),null,'面板挤占后空间不足不能产生负缩放');
});

test('selecting an issue frames its linked messages and wires beside the desktop detail',()=>{
  const previousCSS=global.CSS;global.CSS={escape:id=>id};
  const node=(x,y,width,height)=>({_canvasPosition:{x,y},offsetWidth:width,offsetHeight:height,
    classList:{names:new Set(),add(name){this.names.add(name);},remove(name){this.names.delete(name);}}});
  const first=node(24,100,240,96),last=node(24,1000,240,120),issue=node(328,400,360,220),other=node(1100,20,360,220);
  const nodes={'[data-issue-id="issue"]':issue,'[data-input-id="first"]':first,'[data-input-id="last"]':last};
  const wire={dataset:{target:'issue'},classList:{toggle(name,on){this.active=on;}}};
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
    nodes:{querySelector:key=>nodes[key],querySelectorAll:()=>[first,last,issue,other]},
    wires:{querySelectorAll:()=>[wire]},detail:{hidden:false,offsetWidth:410},
    viewport:{getBoundingClientRect:()=>({width:1200,height:650})},mobileLayout:false,
    transform:{scale:.2,x:0,y:0},applyTransform(){},
    lastRecords:{issues:[{issue:{id:'issue'},bubbles:[{input:{inputId:'first'},node:first},{input:{inputId:'last'},node:last}]}]},
  });
  try{
    controller.focusItem('issue');
    const {scale,x,y}=controller.transform;
    for(const item of [first,last,issue]){
      const p=item._canvasPosition;
      assert.ok(x+p.x*scale>=24-1e-6&&x+(p.x+item.offsetWidth)*scale<=1200-410-24+1e-6,'节点不能被详情面板遮住');
      assert.ok(y+p.y*scale>=24-1e-6&&y+(p.y+item.offsetHeight)*scale<=650-24+1e-6,'拖动过的关联消息也应完整可见');
      assert.ok(item.classList.names.has('team-canvas-related'));
    }
    assert.equal(controller.focusedIssueID,'issue');assert.equal(wire.classList.active,true);
    assert.equal(other.classList.names.has('team-canvas-related'),false);
    assert.ok(scale>.2,'从总览选中时应放大关联内容');
    const before={...controller.transform};controller.focusItem('missing');
    assert.deepEqual(controller.transform,before,'不存在的事项不能移动画布');
    controller.mobileLayout=true;controller.viewport.getBoundingClientRect=()=>({width:390,height:500});
    controller.focusItem('issue');assert.ok(controller.transform.scale>0,'手机详情覆盖画布，不应减去全屏面板宽度');
  }finally{global.CSS=previousCSS;}
});

test('issue details select the relationship and link back using original message numbers',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const node=()=>({children:[],listeners:{},classList:{add(){},remove(){}},append(...items){this.children.push(...items);},replaceChildren(){this.children=[];},addEventListener(type,fn){this.listeners[type]=fn;},setAttribute(){},focus(){}});
  const elements={};global.document={createElement:node,getElementById:id=>elements[id]||(elements[id]=node())};
  global.window={matchMedia:()=>({matches:true})};
  const selected=[],jumped=[],issue={id:'issue',title:'事项',inputs:[{inputId:'input',sourceText:'片段1'},{inputId:'input',sourceText:'片段2'}]};
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
    current:{id:'team'},detail:node(),renderDetail:TeamCanvasController.prototype.renderDetail,
    focusItem:id=>selected.push(id),focusMessage:id=>jumped.push(id),request:async()=>issue,
    lastRecords:{issues:[{issue,bubbles:[{input:{inputId:'input',sequence:7,text:'原始消息'},index:1}]}]},
  });
  try{
    controller.openDetail(issue,node());await Promise.resolve();
    assert.deepEqual(selected,['issue'],'打开详情必须同步选择关联节点');
    const section=elements['team-canvas-detail-body'].children.find(n=>n.children[0]?.textContent==='关联原话');
    assert.equal(section.children.length,2,'同一消息的多个片段只提供一个定位入口');
    const source=section.children[1];assert.equal(source.textContent,'#07 · 原始消息');
    source.listeners.click();assert.deepEqual(jumped,['input']);assert.equal(controller.detail.hidden,true,'手机点击原话应关闭遮挡画布的详情');
  }finally{global.document=previousDocument;global.window=previousWindow;}
});

test('issue deletion confirms, calls DELETE and refreshes the canvas',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const button={disabled:false,textContent:''};
  const errorBox={textContent:''};
  global.document={getElementById:id=>id==='team-canvas-delete-issue'?button:errorBox};
  global.window={confirm:()=>true};
  try{
    let closed=0,refreshed=0,request;
    const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
      current:{id:'team-1'},detailIssue:{id:'issue-1',title:'旧事项'},error:{textContent:''},
      closeDetail:()=>{closed++;},refresh:async()=>{refreshed++;},
      request:async(path,options)=>{if(!options)return{id:'issue-1',title:'旧事项',runs:[]};request={path,options};return{ok:true};},
    });
    await controller.deleteIssue();
    assert.equal(request.path,'/api/teams/team-1/issues/issue-1');
    assert.equal(request.options.method,'DELETE');
    assert.equal(closed,1);
    assert.equal(refreshed,1);
    assert.equal(button.disabled,false);
    assert.equal(button.textContent,'删除事项与对话');
  }finally{global.document=previousDocument;global.window=previousWindow;}
});

test('deleting a parked issue confirms cancellation, cancels only unfinished linked runs, then deletes',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const button={},errorBox={textContent:''},calls=[],prompts=[];
  global.document={getElementById:id=>id==='team-canvas-delete-issue'?button:errorBox};
  global.window={confirm:message=>{prompts.push(message);return true;}};
  const runs=[{id:'finished',status:'completed'},{id:'waiting',status:'waiting_input',goal:'等待确认'},
    {id:'waiting',status:'waiting_input'},{id:'running',status:'dispatching',goal:'执行中'}];
  const issue={id:'issue',title:'事项',runs:[]}; // 打开详情时还没有未完成 Run。
  let closed=0,refreshed=0;
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{current:{id:'team'},detailIssue:issue,
    closeDetail(){closed++;},refresh:async()=>{refreshed++;},
    request:async(path,options)=>{calls.push([path,options?.method||'GET']);return{...issue,runs};}});
  try{
    await controller.deleteIssue();
    assert.deepEqual(calls,[['/api/teams/team/issues/issue','GET'],['/api/teams/team/runs/waiting/cancel','POST'],
      ['/api/teams/team/runs/running/cancel','POST'],['/api/teams/team/issues/issue','DELETE']]);
    assert.match(prompts[0],/取消以下未结束的 Run/);assert.match(prompts[0],/WAITING/);assert.match(prompts[0],/RUNNING/);
    assert.equal(closed,1);assert.equal(refreshed,1);assert.equal(errorBox.textContent,'');assert.equal(button.disabled,false);
    calls.length=0;global.window.confirm=()=>false;await controller.deleteIssue();
    assert.deepEqual(calls,[['/api/teams/team/issues/issue','GET']],'取消确认时不能更改 Run 或事项');
  }finally{global.document=previousDocument;global.window=previousWindow;}
});

test('deleting one issue never cancels a run shared with another issue',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const button={},errorBox={},calls=[],run={id:'shared',status:'running'};
  global.document={getElementById:id=>id==='team-canvas-delete-issue'?button:errorBox};
  global.window={confirm:()=>{throw Error('shared cancellation must not be offered');}};
  const issue={id:'one',runs:[run]};
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{current:{id:'team'},detailIssue:issue,issues:[issue,{id:'two',runs:[run]}],
    request:async(path,options)=>{calls.push(options?.method||'GET');return issue;}});
  try{await controller.deleteIssue();assert.deepEqual(calls,['GET']);assert.match(errorBox.textContent,/共享运行中的 Run/);}
  finally{global.document=previousDocument;global.window=previousWindow;}
});

test('delete failure stays in the detail and never deletes after a cancellation failure',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const button={},errorBox={textContent:''},issue={id:'issue',runs:[{id:'run',status:'waiting_input'}]},calls=[];
  global.document={getElementById:id=>id==='team-canvas-delete-issue'?button:errorBox};global.window={confirm:()=>true};
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{current:{id:'team'},detailIssue:issue,
    request:async(path,options)=>{calls.push(options?.method||'GET');if(!options)return issue;throw Error('取消失败');}});
  try{
    await controller.deleteIssue();assert.deepEqual(calls,['GET','POST']);assert.equal(errorBox.textContent,'取消失败');
    assert.equal(button.disabled,false);assert.equal(controller.deletingIssue,false);
    controller.request=async(path,options)=>{if(!options)return issue;if(options.method==='DELETE')throw Error('删除失败');return{};};
    await controller.deleteIssue();assert.match(errorBox.textContent,/关联 Run 已取消，但事项尚未删除：删除失败/);
    assert.equal(button.disabled,false);
    controller.request=async(path,options)=>{if(!options)return{...issue,runs:[]};throw Error('后台有新任务');};
    await controller.deleteIssue();assert.equal(errorBox.textContent,'后台有新任务','错误直接显示在详情内');
  }finally{global.document=previousDocument;global.window=previousWindow;}
});

test('a linked run finishing during delete confirmation does not block deletion or cancel an active replacement',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const button={},errorBox={},issue={id:'issue',runs:[{id:'run',status:'waiting_input'}]},calls=[];
  global.document={getElementById:id=>id==='team-canvas-delete-issue'?button:errorBox};global.window={confirm:()=>true};
  let status='completed';
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{current:{id:'team'},detailIssue:issue,
    closeDetail(){},refresh:async()=>{},request:async(path,options)=>{
      calls.push([path,options?.method||'GET']);
      if(path.endsWith('/cancel'))throw Object.assign(Error('状态已改变'),{status:409});
      if(path.endsWith('/runs/run'))return{id:'run',status};return issue;
    }});
  try{
    await controller.deleteIssue();assert.equal(calls.at(-1)[1],'DELETE');
    calls.length=0;status='dispatching';await controller.deleteIssue();
    assert.ok(!calls.some(row=>row[1]==='DELETE'));assert.equal(errorBox.textContent,'状态已改变');
  }finally{global.document=previousDocument;global.window=previousWindow;}
});

test('polling redraws only when team canvas data changes',async()=>{
  const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
    current:{id:'team-1'},generation:1,inputs:[],issues:[],error:{textContent:''},refreshToken:null,refreshQueued:false,
  });
  let inputs=[],issues=[],renders=0;
  controller.request=async url=>url.endsWith('/issues')?{issues}:{inputs};
  controller.render=()=>{renders++;controller.lastRecords={};};
  await controller.refresh();
  assert.equal(renders,1,'空团队也要完成首次渲染');
  inputs=[];issues=[];
  await controller.refresh();
  assert.equal(renders,1,'相同数据不应重建画布');
  issues=[{id:'issue-1',title:'画布',status:'completed',taskCounts:{completed:1,failed:0}}];
  await controller.refresh();
  assert.equal(renders,2,'事项变化应触发重绘');
  inputs=[{inputId:'input-1',text:'修复选中状态',associationStatus:'pending',attachments:[{id:'file-1',name:'spec'}]}];
  await controller.refresh();
  assert.equal(renders,3,'输入变化应触发重绘');
  issues=[{taskCounts:{failed:0,completed:1},status:'completed',title:'画布',id:'issue-1'}];
  inputs=[{attachments:[{name:'spec',id:'file-1'}],associationStatus:'pending',text:'修复选中状态',inputId:'input-1'}];
  await controller.refresh();
  assert.equal(renders,3,'嵌套字段顺序变化不应重建画布');
  issues=[{taskCounts:{failed:1,completed:1},status:'completed',title:'画布',id:'issue-1'}];
  await controller.refresh();
  assert.equal(renders,4,'嵌套状态实际变化应触发重绘');
});
