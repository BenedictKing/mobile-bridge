'use strict';
// @ts-check
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
  clear(){this.rows={};this.localBaseline={};}
  // 本设备系统通知按状态边沿判断：首轮只建立基线；重复待确认、同一终态和
  // 正在查看的聊天不重复弹。返回需要提醒的会话，交给 app.js 的统一出口。
  localNotifications(rows,visible,background){
    this.localBaseline=this.localBaseline||{};
    const events=[];
    for(const row of rows||[]){
      if(!row.id||!row.host)continue;
      const key=this.id(row);
      const pending=Number(row.pendingRequests||0)>0;
      const terminal=row.turnStatus==='completed'||row.turnStatus==='failed'||row.turnStatus==='interrupted';
      const signature=terminal?(row.turnId||'')+':'+row.turnStatus:'';
      const previous=this.localBaseline[key];
      this.localBaseline[key]={pending,signature};
      if(!previous||key===visible||!background)continue;
      const notify=row.notify||{};
      if(notify.requests&&pending&&!previous.pending)events.push({kind:'requests',key:key+':requests',host:row.host,id:row.id});
      if(notify.completion&&signature&&signature!==previous.signature)events.push({kind:row.turnStatus,key:key+':turn:'+signature,host:row.host,id:row.id});
    }
    return events;
  }
}
if(typeof module!=='undefined')module.exports={SessionActivity};
