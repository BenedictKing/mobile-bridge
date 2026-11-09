'use strict';

const test=require('node:test');
const assert=require('node:assert/strict');
const {teamCanvasAssociations,teamCanvasLinkedRecords,teamCanvasInputRunID,teamCanvasFitTransform,teamCanvasOwnerLine,teamCanvasDeliveryRows}=require('../web/team-canvas.js');

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

test('fit all respects tall canvas bounds and centers the complete layout',()=>{
  const viewport={width:1000,height:600},canvas={width:940,height:1600};
  const fit=teamCanvasFitTransform(viewport.width,viewport.height,canvas.width,canvas.height);
  const right=fit.x+canvas.width*fit.scale,bottom=fit.y+canvas.height*fit.scale;
  assert.ok(fit.scale<.48);
  assert.ok(fit.x>=0&&right<=viewport.width);
  assert.ok(fit.y>=0&&bottom<=viewport.height);
  assert.equal(teamCanvasFitTransform(0,600,940,1600),null);
});
