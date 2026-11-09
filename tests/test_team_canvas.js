'use strict';

const test=require('node:test');
const assert=require('node:assert/strict');
const {teamCanvasAssociations,isTeamCanvasInternalMessage}=require('../web/team-canvas.js');

test('canvas drops gateway-generated envelopes instead of showing them as authored bubbles',()=>{
  assert.equal(isTeamCanvasInternalMessage('[Team delegation] generated task context'),true);
  assert.equal(isTeamCanvasInternalMessage('[Team context] generated member context'),true);
  assert.equal(isTeamCanvasInternalMessage('（网关约定）自动附加的工作约定'),true);
  assert.equal(isTeamCanvasInternalMessage('请恢复之前的模型重定向策略'),false);
});

test('canvas links only exact source text to a known run',()=>{
  const messages=[
    {id:'m1',text:'修复团队导航'},
    {id:'m2',text:'  修复团队导航  '},
    {id:'m3',text:'修复团队导航和筛选'},
    {id:'m4',text:'尚无对应 Run'},
    {id:'m5',runId:'run-2',text:'当前 Run 的关联由输入账本确认'},
  ];
  const runs=[
    {id:'run-1',goal:'首条目标',updatedAt:'2026-11-09T09:00:00Z'},
    {id:'run-2',goal:'另一个目标',updatedAt:'2026-11-09T10:00:00Z'},
  ];
  const inputs=[{text:'修复团队导航',targetRunId:'run-1'}];
  const details={'run-2':{tasks:[{prompt:'修复团队导航和筛选'}]}};

  assert.deepEqual(teamCanvasAssociations(messages,inputs,runs,details),{
    m1:'run-1',m2:'run-1',m3:'run-2',m5:'run-2',
  });
});

test('canvas leaves repeated exact goals unlinked without a unique source relation',()=>{
  const associations=teamCanvasAssociations(
    [{id:'m1',text:'继续完善团队页面'}],
    [],
    [
      {id:'older',goal:'继续完善团队页面',updatedAt:'2026-11-08T10:00:00Z'},
      {id:'newer',goal:'继续完善团队页面',updatedAt:'2026-11-09T10:00:00Z'},
    ],
  );
  assert.deepEqual(associations,{});
});
