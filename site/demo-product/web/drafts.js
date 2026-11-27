'use strict';
// @ts-check

// Display-local composer state only. Submission identity remains in BridgePendingCommands.
class ComposerDrafts {
  constructor(storage=null,scope=''){
    /** @type {Storage|null} */ this.storage=storage;
    this.scope=scope;
  }
  configure(scope){this.scope=String(scope||'');}
  key(id,host){
    if(!this.storage||!this.scope||!id||!host)return null;
    return 'draft-v2:'+JSON.stringify([this.scope,host,id]);
  }
  read(key){
    if(!key||!this.storage)return null;
    try{
      const value=JSON.parse(this.storage.getItem(key)||'null');
      return value?.version===1&&typeof value.text==='string'&&Number.isInteger(value.revision)?value:null;
    }catch{return null;}
  }
  load(id,host){return this.read(this.key(id,host));}
  save(id,host,fields){
    const key=this.key(id,host),previous=this.read(key);
    if(!key||!this.storage)return null;
    const text=String(fields.text||'').slice(0,100000);
    const clamp=value=>Math.max(0,Math.min(text.length,Number(value)||0));
    const skills=Array.isArray(fields.skills)?[...new Set(fields.skills.filter(value=>typeof value==='string').map(value=>value.slice(0,500)))].slice(0,8):[];
    const workMode=['default','plan','goal'].includes(fields.workMode)?fields.workMode:'default';
    const draft={version:1,text,selectionStart:clamp(fields.selectionStart),selectionEnd:clamp(fields.selectionEnd),workMode,skills,revision:(previous?.revision||0)+1};
    if(previous&&previous.text===draft.text&&previous.selectionStart===draft.selectionStart&&previous.selectionEnd===draft.selectionEnd&&previous.workMode===draft.workMode&&JSON.stringify(previous.skills)===JSON.stringify(draft.skills))return previous;
    try{this.storage.setItem(key,JSON.stringify(draft));return draft;}catch{return null;}
  }
  confirm(id,host,revision){
    const key=this.key(id,host),saved=this.read(key);
    if(!key||!saved||saved.revision!==revision)return false;
    try{this.storage.removeItem(key);return true;}catch{return false;}
  }
  move(oldId,oldHost,newId,newHost){
    const saved=this.load(oldId,oldHost);
    if(!saved)return false;
    if(!this.save(newId,newHost,saved))return false;
    this.clear(oldId,oldHost);return true;
  }
  clear(id,host){
    const key=this.key(id,host);
    if(!key||!this.storage)return;
    try{this.storage.removeItem(key);}catch{}
  }
  clearAll(){
    if(!this.storage)return;
    try{
      const keys=[];for(let index=0;index<this.storage.length;index++){const key=this.storage.key(index);if(key?.startsWith('draft-v2:'))keys.push(key);}
      for(const key of keys)this.storage.removeItem(key);
    }catch{}
  }
}
if(typeof module!=='undefined')module.exports={ComposerDrafts};
