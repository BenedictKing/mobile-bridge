'use strict';
const repo='https://github.com/BenedictKing/mobile-bridge';
const languageButtons=[...document.querySelectorAll('[data-language]')];
let language='zh';
const translations=[...document.querySelectorAll('[data-en]')].map(node=>({node,zh:node.textContent,en:node.dataset.en}));
const attributes=['alt','aria-label','title'].flatMap(name=>[...document.querySelectorAll('[data-en-'+name+']')].map(node=>({node,name,zh:node.getAttribute(name),en:node.getAttribute('data-en-'+name)})));
const imageSources=[...document.querySelectorAll('[data-en-src]')].map(node=>({node,zh:node.getAttribute('src'),en:node.dataset.enSrc}));
function setLanguage(value,persist=false){
  language=value==='en'?'en':'zh';document.documentElement.lang=language==='en'?'en':'zh-CN';
  for(const item of translations)item.node.textContent=item[language];
  document.querySelector('.hero h1').innerHTML=language==='en'?'Your workspace.<br>Beyond your desk.':'你的工作台，<br>不止在电脑前。';
  for(const item of attributes)item.node.setAttribute(item.name,item[language]);
  for(const item of imageSources)item.node.setAttribute('src',item[language]);
  languageButtons.forEach(button=>button.setAttribute('aria-pressed',String(button.dataset.language===language)));
  document.querySelectorAll('[data-doc-link]').forEach(link=>link.href=language==='en'?repo+'/blob/main/README_EN.md':repo);
  document.querySelectorAll('[data-setup-link]').forEach(link=>{const url=new URL(link.href);url.searchParams.set('lang',language);link.href=url;});
  const title=language==='en'?'Mobile / Bridge | Your multi-agent workspace, across screens':'Mobile / Bridge｜随身接力你的多 Agent 工作台';
  const description=language==='en'?'Continue Codex, Kimi, Claude and other agent work from mobile Web, Android, iOS and desktop browsers: chat, team runs, approvals, account pools and migration.':'从手机 Web、Android、iOS 和电脑浏览器继续 Codex、Kimi、Claude 等多 Agent 的工作：聊天、团队编排、审批、账号池与迁移。';
  document.title=title;document.querySelector('meta[name=description]').content=description;document.querySelector('meta[property="og:title"]').content=title;document.querySelector('meta[property="og:description"]').content=description;
  const canonical='https://BenedictKing.github.io/mobile-bridge/'+(language==='en'?'?lang=en':'');
  document.querySelector('link[rel=canonical]').href=canonical;document.querySelector('meta[property="og:url"]').content=canonical;
  if(window.demoExperienceReady)refreshExperience();if(lightbox.open&&zoomSource)showImage(zoomSource);
  if(persist){try{localStorage.setItem('cmb-site-language',language);}catch{}const url=new URL(location.href);url.searchParams.set('lang',language);history.replaceState(null,'',url);}
}
languageButtons.forEach(button=>button.onclick=()=>setLanguage(button.dataset.language,true));
const lightbox=document.getElementById('lightbox');let zoomSource=null;
function showImage(source){const image=document.getElementById('large-image');image.src=source.src;image.alt=source.alt;document.getElementById('image-caption').textContent=source.alt;}
document.querySelectorAll('[data-zoom]').forEach(button=>button.onclick=()=>{zoomSource=button.querySelector('img');showImage(zoomSource);lightbox.showModal();});
document.getElementById('close-lightbox').onclick=()=>lightbox.close();
lightbox.addEventListener('click',event=>{if(event.target===lightbox){const rect=lightbox.getBoundingClientRect();if(event.clientX<rect.left||event.clientX>rect.right||event.clientY<rect.top||event.clientY>rect.bottom)lightbox.close();}});
const routeButtons=[...document.querySelectorAll('[data-route]')];
function selectRoute(key,focus=false){routeButtons.forEach(button=>{const active=button.dataset.route===key;button.setAttribute('aria-selected',String(active));button.tabIndex=active?0:-1;document.getElementById(button.getAttribute('aria-controls')).hidden=!active;if(active&&focus)button.focus();});}
routeButtons.forEach((button,i)=>{
  button.onclick=()=>selectRoute(button.dataset.route);
  button.onkeydown=event=>{let next;if(event.key==='ArrowRight')next=(i+1)%routeButtons.length;if(event.key==='ArrowLeft')next=(i+routeButtons.length-1)%routeButtons.length;if(event.key==='Home')next=0;if(event.key==='End')next=routeButtons.length-1;if(next!==undefined){event.preventDefault();selectRoute(routeButtons[next].dataset.route,true);}};
});
let initial=new URL(location.href).searchParams.get('lang');
if(!['zh','en'].includes(initial)){try{initial=localStorage.getItem('cmb-site-language');}catch{}if(!['zh','en'].includes(initial))initial=navigator.language.toLowerCase().startsWith('zh')?'zh':'en';}
setLanguage(initial);

function openCloudflareGuide(){if(location.hash==='#cloudflare-guide'){selectRoute('cloudflare');document.getElementById('cloudflare-guide').scrollIntoView();}}
window.addEventListener('hashchange',openCloudflareGuide);openCloudflareGuide();

const demoFrame=document.getElementById('product-demo');
const deviceStage=document.getElementById('device-panel');
const deviceButtons=[...document.querySelectorAll('[data-device]')];
let demoPlatform='ios';
const experienceCopy={
 ios:{name:'iOS APP · PREVIEW',zh:['把电脑上的进度，带在身上。','保存几台电脑的网关地址，随时进入同一条聊天。审批、团队进度与账号额度都在同一屏，更多菜单里还有通知和灵动岛示意。',['点选一台电脑，进入聊天','用下方场景跳到团队或审批','打开更多，看通知与设置'],'iOS 预览需自行签名；后台状态可能滞后。'],en:['Carry your computer’s progress with you.','Save a few gateway addresses and pick up the same chat. Approvals, team progress and account quota share one screen; More adds notifications and a Live Activity preview.',['Choose a computer and open a chat','Use the scene bar for Team or Approvals','Open More for notifications and settings'],'The iOS preview requires your own signing. Background state may be stale.']},
 android:{name:'ANDROID APP · PREVIEW',zh:['随身接力，操作更直接。','保存网关地址后直接进聊天、处理审批、看团队进度。紧凑的界面，把空间留给当前任务。',['选择一台已保存的电脑','用下方场景跳到一条审批','切到团队场景看阶段轨道'],'Android 提供 APK 安装包；当前以前台通知为主。'],en:['Pick up your work, wherever you are.','Save a gateway address, then chat, answer approvals and follow team progress. A compact interface leaves room for the task.',['Choose a saved computer','Jump to an approval from the scene bar','Switch to Team for the workflow track'],'An Android APK is available. Notifications currently focus on foreground use.']},
 web:{name:'MOBILE WEB · PREVIEW',zh:['打开浏览器，就到工作现场。','不用安装 App。连上网关后，手机浏览器也能聊天、审批、看团队进度、切换账号与迁移会话。',['在输入框补充要求','点审批卡看风险与原因','打开账号与接入切换账号'],'演示为手机浏览器布局；实际访问需连通电脑网关。'],en:['Open a browser. Get back to work.','No App installation needed. With a reachable gateway, a mobile browser can chat, approve requests, follow team progress, switch accounts and migrate a chat.',['Add an instruction in the composer','Open the approval card for risk and reasons','Switch accounts in Accounts'],'This illustrates a mobile browser. Real use requires a reachable computer gateway.']},
 desktop:{name:'DESKTOP WEB · PREVIEW',zh:['一边对话，一边把事分出去。','宽屏把聊天列表、会话与 Workflow 收在一栏视野里；详情、账号与迁移按需弹出。',['左侧保留聊天列表','在聊天里跟进团队任务','打开迁移，把会话换给另一个 Agent'],'窄屏会自动保持单栏。'],en:['Keep the conversation beside the work.','A wide screen keeps the chat list, the conversation and the workflow track in view; details, accounts and migration open on demand.',['Keep the chat list on the left','Follow the team run in the chat','Open migration to move the chat to another agent'],'Narrow screens stay in a single pane.']}
};
function refreshExperience(){
 const copy=experienceCopy[demoPlatform][language];
 document.getElementById('stage-title').textContent=copy[0];document.getElementById('stage-description').textContent=copy[1];document.getElementById('stage-steps').replaceChildren(...copy[2].map(text=>{const li=document.createElement('li');li.textContent=text;return li;}));document.getElementById('stage-boundary').textContent=copy[3];document.getElementById('device-name').textContent=experienceCopy[demoPlatform].name;
 demoFrame.title=language==='en'?'Bridge '+experienceCopy[demoPlatform].name+' interactive demo':'Bridge '+experienceCopy[demoPlatform].name+' 交互演示';
 demoFrame.contentWindow?.postMessage({type:'bridge-demo-language',lang:language},'*');
}
window.demoExperienceReady=true;
function selectDevice(key,scene,focus=false){
 if(!experienceCopy[key])return;
 demoPlatform=key;deviceStage.dataset.platform=key;deviceStage.setAttribute('aria-labelledby','device-'+key);
 deviceButtons.forEach(b=>{const active=b.dataset.device===key;b.setAttribute('aria-selected',String(active));b.tabIndex=active?0:-1;if(active&&focus)b.focus();});
 const url=new URL('./demo.html',location.href);url.searchParams.set('platform',key);url.searchParams.set('lang',language);if(scene)url.searchParams.set('scene',scene);
 demoFrame.src=url.href;refreshExperience();
}
deviceButtons.forEach((b,i)=>{b.onclick=()=>selectDevice(b.dataset.device);b.onkeydown=e=>{let next;if(e.key==='ArrowRight')next=(i+1)%deviceButtons.length;if(e.key==='ArrowLeft')next=(i+deviceButtons.length-1)%deviceButtons.length;if(e.key==='Home')next=0;if(e.key==='End')next=deviceButtons.length-1;if(next!==undefined){e.preventDefault();selectDevice(deviceButtons[next].dataset.device,undefined,true);}};});
function showDemoScene(scene,scroll=false){demoFrame.contentWindow?.postMessage({type:'bridge-demo-scene',scene},'*');if(scroll)document.getElementById('experience').scrollIntoView({behavior:matchMedia('(prefers-reduced-motion: reduce)').matches?'instant':'smooth',block:'start'});}
document.querySelectorAll('[data-scene]').forEach(b=>b.onclick=()=>showDemoScene(b.dataset.scene));
document.querySelectorAll('[data-demo-jump]').forEach(b=>b.onclick=()=>showDemoScene(b.dataset.demoJump,true));
demoFrame.onload=refreshExperience;
window.addEventListener('message',e=>{if(e.source!==demoFrame.contentWindow||e.origin!=='null'||e.data?.type!=='bridge-demo-handoff')return;selectDevice('ios','chat');});
selectDevice('ios');

document.documentElement.classList.add('js');
if(!matchMedia('(prefers-reduced-motion: reduce)').matches&&'IntersectionObserver'in window){
 const groups=[['.section-head','rise'],['.v2-feature','rise'],['.mobile-feature-grid article','rise'],['.preview-install-grid article','rise'],['.capability','rise'],['.faq details','rise'],['.support>*','rise'],['.media-shell','settle'],['.account-gallery figure','settle'],['.steps li','slide'],['.download','slide']];
 const items=[];
 for(const [selector,mode]of groups)document.querySelectorAll(selector).forEach((node,i)=>{if(node.dataset.reveal)return;node.dataset.reveal=mode;node.style.setProperty('--delay',(i%4)*70+'ms');items.push(node);});
 const observer=new IntersectionObserver(entries=>{for(const entry of entries)if(entry.isIntersecting){entry.target.classList.add('is-in');observer.unobserve(entry.target);}},{rootMargin:'0px 0px -10% 0px',threshold:.08});
 items.forEach(node=>observer.observe(node));
 const heroMedia=document.querySelector('.hero-media');
 const syncHeroMotion=()=>heroMedia.classList.toggle('is-paused',document.hidden||!heroMedia.isIntersecting);
 new IntersectionObserver(entries=>{heroMedia.isIntersecting=entries[0].isIntersecting;syncHeroMotion();}).observe(heroMedia);
 document.addEventListener('visibilitychange',syncHeroMotion);
}
