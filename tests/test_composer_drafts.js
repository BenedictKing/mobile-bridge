'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {ComposerDrafts}=require('../web/drafts.js');
const {WorkModes}=require('../web/modes.js');
const {MemoryStorage}=require('./helpers/pending_commands.js');

const fields=(text='hello',extra={})=>({text,selectionStart:2,selectionEnd:5,workMode:'plan',skills:['review','build'],...extra});

test('draft persists complete composer state without rewriting unchanged revisions',()=>{
  const drafts=new ComposerDrafts(new MemoryStorage(),'gateway-1');
  const first=drafts.save('chat','local',fields());
  assert.equal(first.revision,1);
  assert.deepEqual(drafts.save('chat','local',fields()),first);
  const loaded=drafts.load('chat','local');
  assert.equal(loaded.text,'hello');assert.equal(loaded.selectionStart,2);assert.equal(loaded.selectionEnd,5);
  assert.equal(loaded.workMode,'plan');assert.deepEqual(loaded.skills,['review','build']);
});

test('draft keys isolate gateway, host and session',()=>{
  const drafts=new ComposerDrafts(new MemoryStorage(),'gateway-1');
  drafts.save('chat','local',fields());
  assert.equal(drafts.load('other','local'),null);
  assert.equal(drafts.load('chat','remote'),null);
  drafts.configure('gateway-2');assert.equal(drafts.load('chat','local'),null);
});

test('only the submitted revision is cleared after acknowledgement',()=>{
  const drafts=new ComposerDrafts(new MemoryStorage(),'gateway-1');
  const submitted=drafts.save('chat','local',fields('before'));
  const edited=drafts.save('chat','local',fields('after'));
  assert.equal(drafts.confirm('chat','local',submitted.revision),false);
  assert.equal(drafts.load('chat','local').text,'after');
  assert.equal(drafts.confirm('chat','local',edited.revision),true);
  assert.equal(drafts.load('chat','local'),null);
});

test('stale-chat moves preserve the draft and clearAll leaves unrelated settings alone',()=>{
  const storage=new MemoryStorage(),drafts=new ComposerDrafts(storage,'gateway-1');
  storage.setItem('bridge-appearance','dark');
  const saved=drafts.save('old','local',fields());
  assert.equal(drafts.move('old','local','new','remote'),true);
  assert.equal(drafts.load('old','local'),null);assert.equal(drafts.load('new','remote').text,'hello');
  assert.equal(saved.revision,1);
  drafts.clearAll();
  assert.equal(drafts.load('new','remote'),null);assert.equal(storage.getItem('bridge-appearance'),'dark');
});

test('invalid fields and unavailable storage never invent a saved draft',()=>{
  const storage=new MemoryStorage(),drafts=new ComposerDrafts(storage,'gateway-1');
  const saved=drafts.save('chat','local',fields('abc',{selectionStart:99,selectionEnd:-2,workMode:'invalid',skills:['ok','ok',42]}));
  assert.equal(saved.selectionStart,3);assert.equal(saved.selectionEnd,0);assert.equal(saved.workMode,'default');assert.deepEqual(saved.skills,['ok']);
  const broken={getItem(){throw Error('blocked');},setItem(){throw Error('blocked');},removeItem(){throw Error('blocked');}};
  const unavailable=new ComposerDrafts(broken,'gateway-1');
  assert.equal(unavailable.save('chat','local',fields()),null);assert.equal(unavailable.load('chat','local'),null);
  assert.equal(unavailable.confirm('chat','local',1),false);assert.equal(unavailable.move('a','local','b','local'),false);
  unavailable.clearAll();
});

test('work mode changes notify the composer draft owner',()=>{
  global.BridgeI18n={t:value=>value};
  const changes=[],option={disabled:false,hidden:false},control={querySelectorAll:()=>[],setAttribute(){}};
  const select={value:'default',querySelector:()=>option};
  const modes=new WorkModes({select,control,goalRoot:{firstElementChild:null,replaceChildren(){},querySelector(){return null;}},goalToggle:{},storage:new MemoryStorage(),onChange:value=>changes.push(value)});
  modes.open('chat');select.value='plan';select.onchange();
  assert.deepEqual(changes,['plan']);assert.equal(modes.storage.getItem('work-mode:chat'),'plan');
});
