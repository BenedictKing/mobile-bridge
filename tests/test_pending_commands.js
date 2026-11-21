'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const {PendingCommands,MemoryStorage:Storage}=require('./helpers/pending_commands.js');
test('ACK loss and an active-to-idle transition preserve the complete original intent',()=>{
  const storage=new Storage(),first=new PendingCommands(storage);
  const payload={id:'old',text:'hello',mode:'queue',workMode:'default',skills:['skill'],attachments:[{id:'file'}]};
  first.prepare('pending:local|thread',payload);
  const restarted=new PendingCommands(storage);
  assert.deepEqual(restarted.prepare('pending:local|thread',{...payload,id:'new',mode:'send'}),payload);
  assert.throws(()=>restarted.prepare('pending:local|thread',{...payload,id:'other',text:'different'}),/尚未确认/);
});
test('separate message keys survive two tabs and stale ACKs do not delete another intent',()=>{
  const storage=new Storage(),one=new PendingCommands(storage),two=new PendingCommands(storage);
  one.save('pending:thread',{id:'one',text:'a'});two.save('pending:thread',{id:'two',text:'b'});
  one.remove('pending:thread','one');assert.deepEqual(two.entries('pending:thread'),[{id:'two',text:'b'}]);
});
test('legacy pending messages migrate without changing their identity',()=>{
  const storage=new Storage(),legacy=new Storage();legacy.setItem('pending:thread',JSON.stringify({id:'old',text:'a',mode:'send'}));
  const pending=new PendingCommands(storage,legacy);
  assert.equal(pending.entries('pending:thread')[0].id,'old');assert.equal(legacy.getItem('pending:thread'),null);
  assert.equal(new PendingCommands(storage).entries('pending:thread')[0].id,'old');
});
test('unwritable or damaged persistence fails before a new command is returned',()=>{
  const storage=new Storage();storage.setItem=()=>{throw Error('quota')};
  assert.throws(()=>new PendingCommands(storage).prepare('pending:thread',{id:'one',text:'a'}),/尚未提交/);
  const damaged=new Storage(),pending=new PendingCommands(damaged);damaged.setItem(pending.key('thread')+'one','broken');
  assert.throws(()=>pending.prepare('thread',{id:'two',text:'a'}),/无法读取/);
});
test('logout clears only this application pending namespace',()=>{
  const storage=new Storage(),pending=new PendingCommands(storage);storage.setItem('unrelated','keep');pending.save('thread',{id:'one'});pending.clear();assert.equal(storage.getItem('unrelated'),'keep');assert.deepEqual(pending.entries('thread'),[]);
});
