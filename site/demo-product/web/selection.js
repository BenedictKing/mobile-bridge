'use strict';
// 选中一条消息里的文字后浮出「评论」按钮：点击即把选区转成 Markdown 引用
// 写进输入框并聚焦，省掉「复制 → 粘贴 → 敲意见」。选区只在同一条消息内
// 成立：跨消息的选区没有唯一归属，引用给 Agent 会指代不清，因此不出现。
const BridgeSelection=(()=>{
  const MAX_CHARS=4000, PREFIX='> ';

  // 归一化只清掉选区两端多余的行与尾部空白；首行缩进属于原文，必须留着。
  function normalize(text){
    return String(text??'').replace(/\r\n?/g,'\n').replace(/^[ \t]*\n+/,'').replace(/\s+$/,'');
  }
  // 超长选区在换行处截断；末尾留省略号，读的人一眼知道引用不完整。
  function clampText(text){
    const value=normalize(text);
    if(value.length<=MAX_CHARS)return {text:value,truncated:false};
    const head=value.slice(0,MAX_CHARS),at=head.lastIndexOf('\n');
    return {text:(at>MAX_CHARS*0.6?head.slice(0,at):head)+'\n…',truncated:true};
  }
  // 空行写成单个 >，否则 Markdown 会在那里断开引用块。
  function quoteBlock(text){
    return normalize(text).split('\n').map(line=>line.trim()?PREFIX+line:'>').join('\n');
  }
  // 已有草稿时把引用接在末尾：用户的意见照旧写在引用之后。
  function appendQuote(existing,quote){
    const before=String(existing??'').replace(/\s+$/,'');
    return (before?before+'\n\n':'')+quote+'\n\n';
  }
  function elementOf(node){
    if(!node)return null;
    return node.nodeType===1?node:node.parentElement||null;
  }

  function attach({root,textarea,notify,enabled}){
    if(!root||!textarea)return null;
    const bar=document.createElement('div');
    bar.className='selection-bar';bar.hidden=true;bar.setAttribute('role','toolbar');
    const comment=document.createElement('button');
    comment.type='button';comment.className='selection-comment';
    // data-i18n 让切换语言时 BridgeI18n.apply() 就地刷新，无需额外接线。
    comment.dataset.i18n='评论';
    comment.dataset.i18nAriaLabel='引用选中的文字并评论';
    comment.dataset.i18nTitle='引用选中的文字并评论';
    bar.append(comment);document.body.append(bar);
    const t=source=>typeof BridgeI18n==='undefined'?source:BridgeI18n.t(source);

    let current=null,timer=0,armedUntil=0;
    const usable=()=>typeof enabled==='function'?!!enabled():true;
    function hide(){current=null;bar.hidden=true;}

    // 只接受「同一条消息正文内」的选区；按钮、操作栏和跨消息一律不认。
    function inspect(){
      const selection=window.getSelection?.();
      if(!selection||selection.isCollapsed||selection.rangeCount!==1)return null;
      const range=selection.getRangeAt(0);
      const start=elementOf(range.startContainer),end=elementOf(range.endContainer);
      if(!start||!end||!root.contains(start)||!root.contains(end))return null;
      if(start.closest('button')||end.closest('button'))return null;
      if(start.closest('.message-actions')||end.closest('.message-actions'))return null;
      const row=start.closest('[data-key]');
      if(!row||row!==end.closest('[data-key]'))return null;
      if(!start.closest('.message-body,.activity-body'))return null;
      const {text,truncated}=clampText(selection.toString());
      if(!text)return null;
      const rect=range.getBoundingClientRect();
      if(!rect.width&&!rect.height)return null;
      return {text,truncated,rect};
    }

    // 优先贴在选区下方；下方放不下就翻到上方，横向夹在视口内。
    function place(rect){
      bar.hidden=false;
      const gap=8,margin=10,width=bar.offsetWidth,height=bar.offsetHeight;
      const left=Math.max(margin,Math.min(window.innerWidth-width-margin,rect.left+rect.width/2-width/2));
      const below=rect.bottom+gap;
      const top=below+height<=window.innerHeight-margin?below:Math.max(margin,rect.top-height-gap);
      bar.style.left=left+'px';bar.style.top=top+'px';
    }
    function refresh(){
      if(!usable()){hide();return;}
      const found=inspect();
      if(!found){hide();return;}
      current=found;place(found.rect);
    }
    // 拖手柄时 selectionchange 连发，等手停下来再定位，避免浮条追着手指跳。
    const schedule=()=>{clearTimeout(timer);timer=setTimeout(refresh,160);};

    function insert(picked){
      if(textarea.readOnly||textarea.disabled){notify?.(t('这条聊天当前只读，不能引用评论'));return;}
      textarea.value=appendQuote(textarea.value,quoteBlock(picked.text));
      // dispatchEvent 让既有的 draft 保存、发送按钮同步与高度自适应一起跑。
      textarea.dispatchEvent(new Event('input',{bubbles:true}));
      const body=document.getElementById('composer-body');
      if(body&&body.hidden)document.getElementById('composer-toggle')?.click();
      textarea.focus();
      const end=textarea.value.length;
      try{textarea.setSelectionRange(end,end);}catch{}
      textarea.scrollIntoView({block:'nearest'});
      window.getSelection?.()?.removeAllRanges();
      if(picked.truncated)notify?.(t('选中的内容较长，只引用了前面一部分'));
    }

    const onSelectionChange=()=>{
      const selection=window.getSelection?.();
      if(!selection||selection.isCollapsed){
        // 点浮条会先清掉选区再派发 click：armed 期间保留快照，动作照常生效。
        if(Date.now()>=armedUntil)hide();
        return;
      }
      schedule();
    };
    document.addEventListener('selectionchange',onSelectionChange);
    // 手机长按选词后 selectionchange 未必立刻跟上手指，touchend 再补一次定位。
    root.addEventListener('touchend',schedule,{passive:true});
    document.addEventListener('pointerdown',event=>{if(!bar.contains(event.target))hide();},true);
    document.addEventListener('scroll',()=>{if(Date.now()>=armedUntil)hide();},{capture:true,passive:true});
    window.addEventListener('resize',hide,{passive:true});
    // 三种按下事件都记一次「刚点过浮条」：老 WebView 不一定派发 pointerdown。
    for(const type of ['pointerdown','touchstart','mousedown'])bar.addEventListener(type,()=>{armedUntil=Date.now()+1200;},{passive:true});
    bar.addEventListener('click',event=>{
      if(!comment.contains(event.target))return;
      const picked=current;
      armedUntil=0;hide();
      if(picked)insert(picked);
    });

    return {close:hide};
  }

  return {attach,quoteBlock,appendQuote,clampText,MAX_CHARS};
})();
if(typeof module!=='undefined')module.exports=BridgeSelection;
