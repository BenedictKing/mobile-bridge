'use strict';

const test=require('node:test');
const assert=require('node:assert/strict');
const {TeamCanvasController,teamCanvasAssociations,teamCanvasLinkedRecords,teamCanvasInputRunID,teamCanvasFitTransform,teamCanvasIssueLayout,teamCanvasOwnerLine,teamCanvasDeliveryRows}=require('../web/team-canvas.js');

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
    review:{verdict:'passed'},
    verify:{configured:true,status:'passed',exitCode:0},
    pendingQuestions:1,
    workspaces:[{workspaceId:'ws-1'}],
  }});
  assert.deepEqual(rows,[
    '导航已完成，验证通过。 · 内容已截断',
    '评审结果 · passed',
    '验证结果 · passed · 退出码 0',
    '待回答问题 · 1',
    '工作区 · ws-1',
  ]);
});

test('canvas wires reference rendered bubble nodes and omit missing source records',()=>{
  const one={input:{inputId:'input-1'},node:{style:{left:'0px'}}};
  const two={input:{inputId:'input-2'},node:{style:{left:'1px'}}};
  const records=teamCanvasLinkedRecords([one,two],[
    {issue:{id:'issue-1'},bubbles:[{input:{inputId:'input-2'}},{input:{inputId:'missing'}}]},
  ]);
  assert.deepEqual(records[0].bubbles,[two]);
  assert.equal(records[0].bubbles[0].node.style.left,'1px');
});

test('desktop issue cards anchor near linked bubbles without overlapping',()=>{
  const bubble=(id,y)=>({input:{inputId:id},node:{offsetHeight:80,_canvasPosition:{y}}});
  const issue=(id,inputID)=>({issue:{id},bubbles:[{input:{inputId:inputID}}],node:{offsetHeight:140}});
  const bubbles=[bubble('input-1',28),bubble('input-2',160),bubble('input-3',292)];
  const issues=[issue('issue-late','input-3'),issue('issue-early','input-1'),issue('issue-orphan','missing')];
  const positions=new Map(teamCanvasIssueLayout(bubbles,issues).map(({record,y})=>[record.issue.id,y]));
  assert.equal(positions.get('issue-early'),28);
  assert.equal(positions.get('issue-late'),262);
  assert.equal(positions.get('issue-orphan'),424);
  assert.ok(positions.get('issue-early')<positions.get('issue-late'),'关联顺序应优先于 Issue 创建顺序');
});

test('fit all respects tall canvas bounds and centers the complete layout',()=>{
  const viewport={width:1000,height:600},canvas={width:940,height:1600};
  const fit=teamCanvasFitTransform(viewport.width,viewport.height,canvas.width,canvas.height);
  const right=fit.x+canvas.width*fit.scale,bottom=fit.y+canvas.height*fit.scale;
  assert.ok(fit.scale<.48);
  assert.ok(fit.x>=0&&right<=viewport.width);
  assert.ok(fit.y>=0&&bottom<=viewport.height);
  assert.equal(teamCanvasFitTransform(0,600,940,1600),null);
});

test('issue deletion confirms, calls DELETE and refreshes the canvas',async()=>{
  const previousDocument=global.document,previousWindow=global.window;
  const button={disabled:false,textContent:''};
  global.document={getElementById:id=>id==='team-canvas-delete-issue'?button:null};
  global.window={confirm:()=>true};
  try{
    let closed=0,refreshed=0,request;
    const controller=Object.assign(Object.create(TeamCanvasController.prototype),{
      current:{id:'team-1'},detailIssue:{id:'issue-1',title:'旧事项'},error:{textContent:''},
      closeDetail:()=>{closed++;},refresh:async()=>{refreshed++;},
      request:async(path,options)=>{request={path,options};return{ok:true};},
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
