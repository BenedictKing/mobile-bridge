'use strict';
class SessionActivity {
  constructor(){this.rows={};}
  id(row){return row.host+'|'+row.id;}
  update(rows,visible=null){
    for(const row of rows){
      const key=this.id(row),previous=this.rows[key];
      if(!row.connected){
        if(previous||row.pending)this.rows[key]={...previous,...row,offline:true};
        continue;
      }
      this.rows[key]={...previous,...row,offline:false,pendingRequests:row.pendingRequests||0};
      if(visible===key)this.rows[key].pending=false;
    }
  }
  read(key){const row=this.rows[key];if(!row?.pending)return false;row.pending=false;return true;}
  indicator(key){
    const row=this.rows[key];if(!row)return null;
    if(row.offline){
      if(row.status!=='active')return row.pending?{kind:row.turnStatus==='completed'?'completed':'ended',label:row.turnStatus==='completed'?'已完成，未查看':row.turnStatus==='failed'?'运行失败，未查看':'已停止，未查看'}:null;
      return {kind:'unknown',label:'运行状态暂不可用'};
    }
    if(row.pendingRequests>0)return {kind:'waiting',label:'待确认'};
    if(row.status==='active')return {kind:'running',label:'运行中'};
    if(row.pending)return {kind:row.turnStatus==='completed'?'completed':'ended',label:row.turnStatus==='completed'?'已完成，未查看':row.turnStatus==='failed'?'运行失败，未查看':'已停止，未查看'};
    return null;
  }
  clear(){this.rows={};}
}
if(typeof module!=='undefined')module.exports={SessionActivity};
