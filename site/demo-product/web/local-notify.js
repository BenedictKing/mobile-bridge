'use strict';
// 本设备系统通知：安卓 App 走 MobileBridgeNative，桌面浏览器走 Notification API。
// 只在页面/应用进程存活时工作（页面关闭后仍由 Bark/ntfy 通道负责），不引入
// Service Worker，也不在未授权、非安全上下文或 WebView 里误用浏览器接口。
class LocalNotify {
  constructor(options={}){
    this.win=options.window||window;
    this.native=this.win.MobileBridgeNative||null;
    this.t=options.t||(text=>text);
    this.onChange=options.onChange||(()=>{});
    this.supported=false;
    this.permission='default';
    if(this.native){
      // 老版本 APK 没有 notify()；能力探测失败按不支持处理，不抛给页面。
      try{
        this.supported=typeof this.native.notify==='function'&&this.native.notifications()==='supported';
        if(this.supported)this.permission=this.native.notificationPermission()||'default';
      }catch{}
    }else{
      const api=this.win.Notification;
      // WebView/非安全上下文不提供可靠的浏览器通知；只有真浏览器才启用。
      this.supported=typeof api==='function'&&this.win.isSecureContext===true;
      if(this.supported)this.permission=api.permission;
    }
    this.enabled=this.readFlag();
    if(this.enabled&&(this.permission==='denied'||!this.supported))this.enabled=false;
  }
  storage(){try{return this.win.localStorage;}catch{return null;}}
  readFlag(){return this.storage()?.getItem('local-notify')==='on';}
  saveFlag(){try{if(this.enabled)this.storage()?.setItem('local-notify','on');else this.storage()?.removeItem('local-notify');}catch{}}
  state(){
    let message='';
    if(!this.supported)message=this.native?this.t('当前安卓 App 版本不支持系统通知，请更新 App'):this.t('此浏览器或访问方式不支持系统通知（需要 HTTPS 或 localhost）');
    else if(this.enabled&&this.permission==='default')message=this.t('已开启，授权后在此设备弹出系统通知');
    else if(this.permission==='denied')message=this.t('系统已拒绝通知，请在浏览器或系统设置中允许');
    else if(!this.enabled)message=this.t('页面或应用保持运行时，在本设备弹出系统通知');
    return {supported:this.supported,enabled:this.enabled,permission:this.permission,message};
  }
  async setEnabled(value){
    if(value&&!this.supported)return false;
    if(!value){
      this.enabled=false;
      if(this.native){try{this.native.setNotificationsEnabled(false);}catch{}}
      this.saveFlag();
      this.onChange(this.state());
      return true;
    }
    if(this.native){
      if(this.permission==='granted'){this.enabled=true;this.saveFlag();this.onChange(this.state());return true;}
      const answer=await new Promise(resolve=>{
        // 安卓权限结果通过事件异步回来；同名事件只消费一次。
        const handler=event=>{
          this.win.removeEventListener('mobilebridge-notification-permission',handler);
          this.permission=event.detail?.permission||'denied';
          resolve(this.permission==='granted');
        };
        this.win.addEventListener('mobilebridge-notification-permission',handler);
        try{this.native.setNotificationsEnabled(true);}
        catch{
          this.win.removeEventListener('mobilebridge-notification-permission',handler);
          resolve(false);
        }
      });
      this.enabled=answer;
      this.saveFlag();this.onChange(this.state());
      return answer;
    }
    if(this.win.Notification.permission==='granted')this.permission='granted';
    else if(this.win.Notification.permission==='denied')this.permission='denied';
    else{
      try{this.permission=await this.win.Notification.requestPermission();}
      catch{this.permission='denied';}
    }
    this.enabled=this.permission==='granted';
    this.saveFlag();this.onChange(this.state());
    return this.enabled;
  }
  focus(id,host){
    if(this.native){
      try{this.native.openChat(host,id);}catch{}
      return;
    }
    // 详情 hash 与 app.js 的路由约定一致；不在聊天上下文时由 app.js 打开。
    this.win.location.hash='#'+id+'~'+encodeURIComponent(host);
    this.win.focus();
  }
  notify({key,title,body,host,id}){
    if(!this.enabled)return;
    if(this.native){
      if(this.permission==='granted'){try{this.native.notify(JSON.stringify({key,title,body,host,id}));}catch{}}
      return;
    }
    if(this.permission!=='granted')return;
    try{
      const notification=new this.win.Notification(title,{body,tag:key,icon:'/icon.png'});
      notification.onclick=()=>{this.focus(id,host);notification.close();};
    }catch{}
  }
}
if(typeof module!=='undefined')module.exports={LocalNotify};
if(typeof window!=='undefined')window.LocalNotify=LocalNotify;
