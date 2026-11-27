'use strict';
// @ts-check

// Independent of the application bundle: a failed app.js must still leave a usable retry page.
function createBootstrap(win,doc,timeout=30000){
  let settled=false,completed=false,failure='',timer=null;
  const $=id=>doc.getElementById(id);
  let english=false;
  try{english=(win.localStorage.getItem('bridge-language')||(win.navigator.language||'')).toLowerCase().startsWith('en');}catch{}
  const text=(zh,en)=>english?en:zh;
  function render(){
    const root=$('bootstrap-status');if(!root)return;
    root.hidden=completed;root.setAttribute('aria-busy',String(!settled));
    if(failure){$('login').hidden=true;$('app').hidden=true;}
    if(!failure)return;
    $('bootstrap-title').textContent=failure==='timeout'?text('连接超时','Connection timed out'):text('工作台未能启动','The workspace could not start');
    $('bootstrap-message').textContent=failure==='timeout'?text('启动等待已超过 30 秒，请检查网络后重新加载。','Startup took more than 30 seconds. Check the connection and reload.'):text('必需的页面资源未能加载。请检查网络后重新加载。','A required page resource did not load. Check the connection and reload.');
    $('bootstrap-retry').hidden=false;
  }
  function fail(){if(settled)return;settled=true;failure='resource';if(timer!==null)win.clearTimeout(timer);timer=null;win.removeEventListener('error',onError,true);render();}
  function ready(){if(completed)return;settled=true;completed=true;failure='';if(timer!==null)win.clearTimeout(timer);timer=null;win.removeEventListener('error',onError,true);render();}
  function onError(event){
    if(settled)return;
    const target=event.target,url=target?.src||target?.href||event.filename;
    if(!url)return;
    try{
      const resource=new URL(url,win.location.href);
      if(resource.origin!==win.location.origin)return;
      if(event.filename||['SCRIPT','LINK'].includes(target?.tagName))fail();
    }catch{}
  }
  function init(){
    const retry=$('bootstrap-retry');if(retry)retry.onclick=()=>win.location.reload();
    render();
    if(!settled)timer=win.setTimeout(()=>{if(!settled){failure='timeout';settled=true;win.removeEventListener('error',onError,true);render();}},timeout);
  }
  win.addEventListener('error',onError,true);
  if(doc.readyState==='loading')doc.addEventListener('DOMContentLoaded',init,{once:true});else init();
  return {ready,fail};
}
var BridgeBootstrap=typeof document==='undefined'?null:createBootstrap(window,document);
if(typeof module!=='undefined')module.exports={createBootstrap};
