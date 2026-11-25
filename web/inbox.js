'use strict';
// 跨 Agent 待处理列表：只读汇总各聊天的待确认请求，点开跳回原聊天处理。
// 回应仍走原聊天的审批卡片（同一 pending request ID 与命令账本），这里不提交任何回应。
const BridgeInbox=(()=>{
  const t=source=>typeof BridgeI18n==='undefined'?source:BridgeI18n.t(source);
  const agentName=kind=>typeof BridgeI18n==='undefined'?kind:BridgeI18n.agentTag(kind)||kind;
  const KIND_LABELS={plan:'计划审阅',question:'Agent 提问',input:'需要回复',permission:'权限确认'};
  function el(tag,className,text){const node=document.createElement(tag);if(className)node.className=className;if(text!==undefined)node.textContent=text;return node;}
  function items(value){
    if(!Array.isArray(value?.items))throw new Error(t('待处理列表格式不正确'));
    return value.items.filter(item=>item&&typeof item.sessionId==='string'&&item.sessionId&&typeof item.host==='string');
  }
  // 同一聊天的多条请求合并为一组，组内保持服务端顺序。
  function group(list){
    const groups=new Map();
    for(const item of list){
      const key=item.host+'|'+item.sessionId;
      if(!groups.has(key))groups.set(key,{key,agent:item.agent||'',host:item.host,sessionId:item.sessionId,title:item.title||'',items:[]});
      groups.get(key).items.push(item);
    }
    return [...groups.values()];
  }
  function row(item){
    const node=el('li','inbox-item');
    const head=el('div','inbox-item-head');
    head.append(el('span','inbox-kind inbox-kind-'+(KIND_LABELS[item.kind]?item.kind:'permission'),t(KIND_LABELS[item.kind]||KIND_LABELS.permission)));
    const version=Number(item.planVersion);
    if(item.kind==='plan'&&Number.isSafeInteger(version)&&version>0)head.append(el('span','inbox-meta',t('版本')+' '+version));
    if(!item.supported)head.append(el('span','inbox-meta',t('需在桌面处理')));
    node.append(head);
    if(item.summary)node.append(el('p','inbox-summary',item.summary));
    return node;
  }
  function render(container,list,open){
    const groups=group(list);
    if(!groups.length){container.replaceChildren(el('p','muted inbox-empty',t('没有待处理的请求')));return 0;}
    const nodes=[];
    for(const entry of groups){
      const card=el('section','inbox-group');
      const button=el('button','inbox-open');button.type='button';
      const title=el('span','inbox-title',entry.title||t('未命名聊天'));
      const meta=el('span','inbox-meta',[agentName(entry.agent),entry.host&&entry.host!=='local'?entry.host:'',entry.items.length+' '+t('项待处理')].filter(Boolean).join(' · '));
      button.append(title,meta);
      // 标题按钮负责键盘与读屏入口；摘要列表不能放进 button，单独绑定同一动作，整张卡都可点。
      const openChat=()=>open(entry.sessionId,entry.host);
      button.onclick=openChat;
      const list=el('ul','inbox-items');
      list.onclick=openChat;
      for(const item of entry.items)list.append(row(item));
      card.append(button,list);
      nodes.push(card);
    }
    container.replaceChildren(...nodes);
    return list.length;
  }
  // 每次打开都向网关取最新快照；迟到的旧结果不覆盖新请求。
  function controller({container,request,open,onCount}){
    let generation=0;
    async function refresh(){
      const current=++generation;
      container.replaceChildren(el('p','muted inbox-empty',t('正在加载…')));
      try{
        const list=items(await request());
        if(current!==generation)return;
        // 先渲染再回调：可选调用会连同实参一起短路，不能把 render 放进 onCount?.() 里。
        const count=render(container,list,open);
        onCount?.(count);
      }catch(error){
        if(current!==generation)return;
        const message=el('p','error',t(error?.message||'加载失败'));
        const retry=el('button','plain',t('重试'));retry.type='button';retry.onclick=()=>refresh();
        container.replaceChildren(message,retry);
      }
    }
    return {refresh};
  }
  // 点遮罩关闭：backdrop 点击落在 dialog 自身；.picker 有内边距，点内边距也是 dialog，
  // 所以再按坐标排除面板矩形内的点击。列表只读，误触无损失。
  function closeOnBackdrop(dialog){
    dialog.addEventListener('click',event=>{
      if(event.target!==dialog)return;
      const box=dialog.getBoundingClientRect();
      if(event.clientX<box.left||event.clientX>box.right||event.clientY<box.top||event.clientY>box.bottom)dialog.close();
    });
  }
  return {items,group,render,controller,closeOnBackdrop};
})();
if(typeof window!=='undefined')window.BridgeInbox=BridgeInbox;
if(typeof module!=='undefined')module.exports=BridgeInbox;
