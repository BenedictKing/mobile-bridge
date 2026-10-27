'use strict';
var BridgeConnection=(()=>{
  let publicCsrf='',publicInstanceId='',candidate='',direct=null,probing=null,timer=null,retryAt=0,failures=0,generation=0;
  const apiPath=path=>typeof path==='string'&&path.startsWith('/api/');
  const safe=method=>!method||method==='GET'||method==='HEAD';
  function notify(){BridgeConnection.onchange?.(!!direct);}
  function schedule(delay){
    if(timer!==null||!candidate||!publicCsrf||direct)return;
    timer=setTimeout(()=>{timer=null;probe();},Math.max(0,delay));
  }
  function deactivate(notifyChange=true){
    const changed=!!direct;direct=null;generation++;
    if(changed&&notifyChange)notify();
    retryAt=Date.now()+1000;failures=0;schedule(1000);
  }
  function reset(){
    generation++;publicCsrf='';publicInstanceId='';candidate='';direct=null;probing=null;retryAt=0;failures=0;
    if(timer!==null)clearTimeout(timer);timer=null;
  }
  function configure(auth){
    candidate=typeof auth?.direct==='string'?auth.direct:'';
    publicInstanceId=typeof auth?.instanceId==='string'?auth.instanceId:'';
    if(!candidate&&direct){direct=null;generation++;notify();}
  }
  function authenticated(value){publicCsrf=value||'';retryAt=0;failures=0;probe();}
  async function probe(){
    if(probing||direct||!candidate||!publicCsrf||Date.now()<retryAt)return probing;
    const origin=candidate,epoch=++generation;
    const attempt=(async()=>{
      const grantResponse=await globalThis.fetch('/api/direct/grant',{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-CSRF-Token':publicCsrf},body:'{}'});
      if(!grantResponse.ok)throw Error('grant');
      const grant=await grantResponse.json();
      if(grant.direct!==origin)throw Error('candidate');
      const exchangeResponse=await globalThis.fetch(origin+'/api/direct/exchange',{method:'POST',credentials:'include',cache:'no-store',headers:{'Content-Type':'application/json'},body:JSON.stringify({token:grant.token})});
      if(!exchangeResponse.ok)throw Error('exchange');
      const exchanged=await exchangeResponse.json();
      const authResponse=await globalThis.fetch(origin+'/api/auth',{credentials:'include',cache:'no-store'});
      const auth=authResponse.ok?await authResponse.json():null;
      if(!auth?.authenticated||auth.instanceId!==publicInstanceId||origin!==candidate)throw Error('identity');
      if(epoch!==generation)return;
      direct={origin,csrf:exchanged.csrf};failures=0;retryAt=0;notify();
    })().catch(()=>{
      if(epoch===generation){retryAt=Date.now()+Math.min(1000*(1<<Math.min(failures++,6)),60000);schedule(retryAt-Date.now());}
    });
    probing=attempt;
    attempt.finally(()=>{if(probing===attempt)probing=null;});
    return attempt;
  }
  function url(path){
    return direct&&apiPath(path)?direct.origin+path:path;
  }
  function isDirect(value){
    if(!candidate||typeof location==='undefined')return false;
    try{return new URL(value,location.href).origin===candidate;}catch{return false;}
  }
  function fallback(value){
    if(!isDirect(value))return value;
    const parsed=new URL(value,location.href);return parsed.pathname+parsed.search+parsed.hash;
  }
  async function request(path,options={}){
    const useDirect=!!direct&&apiPath(path),method=(options.method||'GET').toUpperCase(),endpoint=useDirect?direct:null;
    const headers=new Headers(options.headers||{}),csrf=endpoint?.csrf||publicCsrf;
    if(!safe(method)&&csrf)headers.set('X-CSRF-Token',csrf);
    const requestOptions={...options,method,headers,credentials:endpoint?'include':'same-origin'};
    let response;
    try{
      response=await globalThis.fetch(endpoint?endpoint.origin+path:path,requestOptions);
    }catch(error){
      if(!endpoint||options.signal?.aborted||error?.name==='AbortError')throw error;
      deactivate();
      if(safe(method))return request(path,options);
      const failure=Error('局域网连接中断，请重试');failure.retryable=true;failure.directFallback=true;throw failure;
    }
    if(endpoint&&response.status===401){
      deactivate();
      if(safe(method))return request(path,options);
      const failure=Error('局域网会话已失效，请重试');failure.retryable=true;failure.directFallback=true;throw failure;
    }
    return response;
  }
  function eventSource(path){
    const target=url(path);return new EventSource(target,{withCredentials:target!==path});
  }
  async function logout(){
    const endpoint=direct,publicToken=publicCsrf;direct=null;generation++;notify();
    const requests=[];
    if(endpoint)requests.push(globalThis.fetch(endpoint.origin+'/api/logout',{method:'POST',credentials:'include',headers:{'Content-Type':'application/json','X-CSRF-Token':endpoint.csrf},body:'{}'}));
    if(publicToken)requests.push(globalThis.fetch('/api/logout',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-CSRF-Token':publicToken},body:'{}'}));
    await Promise.allSettled(requests);reset();
  }
  if(typeof document!=='undefined')document.addEventListener('error',event=>{
    const image=event.target;
    if(!(image instanceof HTMLImageElement)||!isDirect(image.src))return;
    const source=fallback(image.src),anchor=image.closest('a');
    if(anchor){if(isDirect(anchor.href))anchor.href=fallback(anchor.href);if(isDirect(anchor.dataset.imagePreview))anchor.dataset.imagePreview=fallback(anchor.dataset.imagePreview);}
    image.dataset.publicFallback='1';deactivate();image.src=source;
  },true);
  return {configure,authenticated,probe,reset,logout,request,url,eventSource,isDirect,fallback,
    wake(){retryAt=0;probe();},disconnected(){deactivate();},unauthenticated(){publicCsrf='';deactivate();},
    trusted(value){if(typeof location==='undefined')return false;try{const origin=new URL(value,location.href).origin;return origin===location.origin||origin===candidate;}catch{return false;}},
    get active(){return !!direct;},get origin(){return direct?.origin||'';},onchange:null};
})();
if(typeof module!=='undefined')module.exports={BridgeConnection};
