'use strict';
// @ts-check
// Each intent has its own storage key: another tab cannot replace its identity.
class PendingCommands{
  constructor(storage,legacy){this.storage=storage;this.legacy=legacy;this.prefix='bridge.pending.v1:';}
  key(context){return this.prefix+encodeURIComponent(context)+':';}
  signature(payload){
    const value={...payload};delete value.id;delete value.commandId;delete value.mode;delete value.uiLocale;
    if(value.attachments)value.attachments=value.attachments.map(item=>typeof item==='string'?item:item.id);
    function canonical(value){if(Array.isArray(value))return value.map(canonical);if(value&&typeof value==='object')return Object.fromEntries(Object.keys(value).sort().map(key=>[key,canonical(value[key])]));return value;}
    return JSON.stringify(canonical(value));
  }
  entries(context){
    const prefix=this.key(context),items=[];
    try{
      const old=this.legacy?.getItem(context);
      if(old){const payload=JSON.parse(old);if(payload){this.save(context,payload);this.legacy.removeItem(context);}}
      for(let i=0;i<this.storage.length;i++){
        const key=this.storage.key(i);if(!key?.startsWith(prefix))continue;
        const item=JSON.parse(this.storage.getItem(key));
        if(item?.version!==1||!(item.payload?.id||item.payload?.commandId))throw Error();
        items.push(item);
      }
    }catch{throw Error('无法读取未确认命令，请检查浏览器存储后重试');}
    return items.sort((a,b)=>a.createdAt-b.createdAt).map(item=>item.payload);
  }
  save(context,payload){
    const id=payload.id||payload.commandId;if(!id)throw Error('命令身份不能为空');
    try{this.storage.setItem(this.key(context)+id,JSON.stringify({version:1,createdAt:Date.now(),payload}));}
    catch{throw Error('无法保存未确认命令，尚未提交，请检查浏览器存储');}
    return payload;
  }
  prepare(context,payload){
    const prior=this.entries(context),match=prior.find(item=>this.signature(item)===this.signature(payload));
    if(match)return match;
    if(prior.length)throw Error('上一条命令尚未确认，请先重试或核实后移除本地记录');
    return this.save(context,payload);
  }
  remove(context,id){
    this.storage.removeItem(this.key(context)+id);
    const old=this.legacy?.getItem(context);if(old){try{const payload=JSON.parse(old);if((payload?.id||payload?.commandId)===id)this.legacy.removeItem(context);}catch{}}
  }
  clear(){const keys=[];for(let i=0;i<this.storage.length;i++){const key=this.storage.key(i);if(key?.startsWith(this.prefix))keys.push(key);}for(const key of keys)this.storage.removeItem(key);}
}
if(typeof module!=='undefined')module.exports={PendingCommands};
if(typeof window!=='undefined'){let storage;try{storage=window.localStorage;}catch{}window.BridgePendingCommands=new PendingCommands(storage,window.sessionStorage);}
