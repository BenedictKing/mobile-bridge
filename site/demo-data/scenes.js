'use strict';
// Scene router for the offline product demo. Every scene is a pure UI state on
// top of the fixture fetch layer in demo-data/web.js.
let sceneRevision=0;
const dialog=id=>document.getElementById(id);
const closeDialogs=()=>document.querySelectorAll('dialog[open]').forEach(node=>node.close());
const scrollTimeline=top=>{const timeline=dialog('timeline');if(timeline)timeline.scrollTop=top?0:timeline.scrollHeight;};
// The app routes itself on start (auth → teams → projects → chat). Opening a
// panel before that settles lets the late openChat close it again.
const settled=async(timeout=5000)=>{
  const deadline=Date.now()+timeout;
  while(Date.now()<deadline){
    const app=dialog('app');
    if(app&&!app.hidden&&document.querySelectorAll('#sessions > *').length)break;
    await new Promise(resolve=>setTimeout(resolve,50));
  }
  await new Promise(resolve=>setTimeout(resolve,120));
};
window.demoScene=async scene=>{
  const revision=++sceneRevision;
  if(!scene||scene==='home'||scene==='list'){
    closeDialogs();
    if(typeof closeDetailView==='function')closeDetailView();else dialog('back')?.click();
    return;
  }
  await settled();
  if(revision!==sceneRevision)return;
  // Dialog scenes settle without a further await, so a superseded call can
  // never close the dialog a newer call just opened.
  if(scene==='accounts'){closeDialogs();dialog('accounts-button')?.click();return;}
  if(scene==='pool'){closeDialogs();dialog('accounts-button')?.click();setTimeout(()=>dialog('pool-tab-workbuddy')?.click(),250);return;}
  if(scene==='settings'){closeDialogs();document.querySelector('[data-open-appearance]')?.click();return;}
  // Chat scenes: reopen explicitly so the scene does not depend on the hash
  // the app started from. Migration needs an idle chat with no pending
  // approvals (same rule the product applies to its own control).
  const chatId=scene==='team'?0:scene==='migrate'?2:1;
  await openChat(window.demoIds[chatId],'local');
  if(revision!==sceneRevision)return;
  closeDialogs();
  if(scene==='team'&&window.demoTeam)openTeamChat(window.demoTeam);
  if(scene==='migrate')dialog('chat-details-button')?.click();
  // Screenshot helper: ?scroll=top opens a chat at its first message.
  const top=new URLSearchParams(location.search).get('scroll')==='top';
  requestAnimationFrame(()=>scrollTimeline(top));
};
// The product scripts are deferred, so the first scene waits for them.
const startScene=new URLSearchParams(location.search).get('scene');
const play=()=>{if(startScene)window.demoScene(startScene);};
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',play,{once:true});else play();
window.addEventListener('message',event=>{
  if(event.source!==parent)return;
  if(event.data?.type==='bridge-demo-scene')window.demoScene(event.data.scene);
});
