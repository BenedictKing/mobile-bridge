'use strict';
const BridgePlanReview=(()=>{
  const t=source=>typeof BridgeI18n==='undefined'?source:BridgeI18n.t(source);
  function metadata(params={}){
    const version=Number(params.planVersion);
    if(typeof params.planId!=='string'||!params.planId||!Number.isSafeInteger(version)||version<1||typeof params.planHash!=='string'||!/^[a-f0-9]{64}$/i.test(params.planHash))return null;
    return {id:params.planId,version,hash:params.planHash};
  }
  // 批注类型与上限需与 internal/planstore 保持一致。
  const TYPES={comment:'批注',delete:'建议删除'},MAX_ANNOTATIONS=50,MAX_BODY=20000;
  // 网关 JSON 请求体上限 512000 字节；修订正文与 planFeedback 会重复批注，留出余量。
  const REQUEST_BUDGET=480000;
  function validAnnotation(item){
    return !!item&&typeof item==='object'&&/^A[1-9]\d{0,2}$/.test(item.id)&&Object.prototype.hasOwnProperty.call(TYPES,item.type)&&typeof item.quote==='string'&&!!item.quote.trim()&&(item.body===undefined||typeof item.body==='string');
  }
  function cleanAnnotations(list){
    return Array.isArray(list)?list.filter(validAnnotation).map(item=>({id:item.id,type:item.type,quote:item.quote,body:item.body||''})):[];
  }
  // 选区 toString 与 textContent 在块级元素之间的空白并不一致，比较时忽略全部空白。
  const squash=text=>String(text??'').replace(/\s+/g,'');
  // 锚点只做文本包含判断：截断标记不属于原文，比较前去掉；找不到就明确标记失效。
  function anchorFound(quote,text){
    const needle=squash(String(quote??'').replace(/\n…$/,''));
    return !!needle&&squash(text).includes(needle);
  }
  const quoteLines=text=>String(text).split('\n').map(line=>line.trim()?'> '+line:'>').join('\n');
  function formatAnnotations(annotations,text){
    const parts=['### Annotations','Address every annotation below and state in your reply how each ID was handled.'];
    for(const item of annotations){
      const body=item.body.trim();
      const instruction=item.type==='delete'?'Remove the quoted content.'+(body?' Reason: '+body:''):body;
      parts.push(`#### ${item.id} · ${item.type==='delete'?'Delete':'Comment'}\n\n${quoteLines(item.quote)}\n\n${instruction}`);
    }
    if(String(text??'').trim())parts.push('### General feedback',text);
    return parts.join('\n\n');
  }
  function formatFeedback(text,params,annotations=[]){
    const meta=metadata(params),items=cleanAnnotations(annotations);
    const body=items.length?formatAnnotations(items,text):text;
    if(!meta)return body;
    return `## Plan revision feedback\n\nPlan-ID: ${JSON.stringify(meta.id)}\nPlan-Version: ${meta.version}\nPlan-SHA256: ${meta.hash}\n\nRevise the plan to address ALL feedback below before resubmitting. Do not resubmit the plan unchanged.\n\n${body}`;
  }
  function utf8Length(text){
    try{return encodeURIComponent(text).replace(/%[0-9A-F]{2}/g,'x').length;}catch{return text.length*3;}
  }
  function versionBadge(params){
    const meta=metadata(params);
    if(!meta)return null;
    const badge=document.createElement('p');badge.className='plan-revision-meta';
    badge.textContent=t('版本')+' '+meta.version+' · '+meta.hash.slice(0,12)+'…';
    badge.title=meta.id+'\n'+meta.hash;
    return badge;
  }
  function foldRows(rows,context=2,minHidden=4){
    const segments=[];
    for(let index=0;index<rows.length;){
      if(rows[index].kind!=='context'){segments.push({kind:'rows',rows:[rows[index++]]});continue;}
      const start=index;
      while(index<rows.length&&rows[index].kind==='context')index++;
      const run=rows.slice(start,index);
      if(run.length<context*2+minHidden){segments.push({kind:'rows',rows:run});continue;}
      if(context)segments.push({kind:'rows',rows:run.slice(0,context)});
      segments.push({kind:'fold',rows:run.slice(context,run.length-context)});
      if(context)segments.push({kind:'rows',rows:run.slice(run.length-context)});
    }
    return segments;
  }
  function diffLine(row){
    const line=document.createElement('div');line.className='diff-line '+row.kind;
    const oldNo=document.createElement('span'),newNo=document.createElement('span'),code=document.createElement('code');
    oldNo.className=newNo.className='diff-no';oldNo.textContent=row.oldNo??'';newNo.textContent=row.newNo??'';code.textContent=row.text;
    line.append(oldNo,newNo,code);return line;
  }
  function appendRows(target,rows){for(const row of rows)target.append(diffLine(row));}
  function diffBlock(previous,current,label){
    if(typeof previous!=='string'||typeof current!=='string'||previous===current||!window.BridgeDiff?.pairRows)return null;
    const result=window.BridgeDiff.pairRows(previous,current),rows=result.rows;
    const additions=rows.filter(row=>row.kind==='add').length,deletions=rows.filter(row=>row.kind==='del').length;
    const details=document.createElement('details');details.className='plan-revision-diff';
    const summary=document.createElement('summary');summary.append(t(label)+' · ');
    const added=document.createElement('span');added.className='diff-stat-add';added.textContent='+'+additions;
    const deleted=document.createElement('span');deleted.className='diff-stat-del';deleted.textContent='−'+deletions;
    summary.append(added,' ',deleted);details.append(summary);
    if(result.degraded){const note=document.createElement('p');note.className='diff-note';note.textContent=t('内容较大，未做精确对齐');details.append(note);}
    const view=document.createElement('div');view.className='diff-view plan-revision-diff-view';
    for(const segment of foldRows(rows)){
      if(segment.kind==='rows'){appendRows(view,segment.rows);continue;}
      const folded=document.createElement('details');folded.className='plan-revision-context';
      const foldedSummary=document.createElement('summary');foldedSummary.textContent=t('展开')+' '+segment.rows.length+' '+t('行未变更内容');
      folded.append(foldedSummary);appendRows(folded,segment.rows);view.append(folded);
    }
    details.append(view);return details;
  }
  function diffDetails(params){
    const meta=metadata(params);
    if(!meta||meta.version<2)return null;
    return diffBlock(params.previousPlanContent,params.planContent,'查看与上一版的差异');
  }
  function historyDetails(params,load,renderContent){
    const meta=metadata(params);
    if(!meta||typeof load!=='function')return null;
    const details=document.createElement('details');details.className='plan-history';
    const summary=document.createElement('summary');summary.textContent=t('历史版本');
    const status=document.createElement('p');status.className='muted';
    details.append(summary,status);
    let loading=false,loaded=false;
    details.ontoggle=async()=>{
      if(!details.open||loading||loaded)return;
      loading=true;status.className='muted';status.textContent=t('正在加载历史版本…');
      try{
        const result=await load();
        if(!result||result.planId!==meta.id||!Array.isArray(result.revisions))throw Error(t('历史版本数据无效'));
        const revisions=result.revisions.filter(revision=>Number.isSafeInteger(revision?.version)&&revision.version>0&&typeof revision.hash==='string'&&/^[a-f0-9]{64}$/i.test(revision.hash)&&typeof revision.content==='string').sort((a,b)=>b.version-a.version);
        if(revisions.length!==result.revisions.length)throw Error(t('历史版本数据无效'));
        status.textContent=revisions.length?'':t('暂无历史版本');
        const list=document.createElement('div');list.className='plan-history-list';
        for(const revision of revisions){
          const item=document.createElement('details');item.className='plan-history-version';
          const title=document.createElement('summary'),date=new Date(revision.createdAt);
          title.textContent=t('版本')+' '+revision.version+' · '+revision.hash.slice(0,12)+'…'+(Number.isNaN(date.getTime())?'':' · '+date.toLocaleString());
          item.append(title);
          let rendered=false;
          item.ontoggle=()=>{
            if(!item.open||rendered)return;rendered=true;
            const body=document.createElement('div');body.className='approval-markdown plan-history-content';
            if(typeof renderContent==='function')renderContent(body,revision.content);else body.textContent=revision.content;
            item.append(body);
            if(revision.version!==meta.version||revision.hash!==meta.hash){const diff=diffBlock(revision.content,params.planContent,'与当前版本的差异');if(diff)item.append(diff);}
            const feedback=cleanFeedback(revision.feedback);
            if(feedback.length){const heading=document.createElement('p');heading.className='plan-feedback-title';heading.textContent=t('该版收到的反馈');item.append(heading,feedbackList(feedback));}
          };
          list.append(item);
        }
        details.append(list);loaded=true;
      }catch(error){status.className='error';status.textContent=t('历史版本加载失败，请收起后重试')+'：'+error.message;}
      finally{loading=false;}
    };
    return details;
  }
  function sessionStore(){try{return globalThis.sessionStorage||null;}catch{return null;}}
  // 计划卡片的批注编辑器：选区浮条生成批注，修改意见框作为全局意见；
  // 草稿按调用方给定的 key 存在当前标签页，卡片重绘或整页刷新后恢复。
  function reviewer({root,textarea,params,storageKey,notify,onChange}){
    if(!root||!textarea)return null;
    const storage=storageKey?sessionStore():null;
    let annotations=[];
    try{
      const saved=storage?JSON.parse(storage.getItem(storageKey)||'null'):null;
      if(saved&&typeof saved==='object'){
        annotations=cleanAnnotations(saved.annotations).slice(0,MAX_ANNOTATIONS);
        if(!textarea.value&&typeof saved.text==='string')textarea.value=saved.text;
      }
    }catch{}
    const panel=document.createElement('div');panel.className='plan-annotations';
    const title=document.createElement('p');title.className='plan-annotations-title';
    const list=document.createElement('div');list.className='plan-annotation-list';
    panel.append(title,list);
    const inputs=new Map();
    function save(){
      if(!storage)return;
      try{
        if(annotations.length||textarea.value)storage.setItem(storageKey,JSON.stringify({text:textarea.value,annotations}));
        else storage.removeItem(storageKey);
      }catch{}
    }
    function changed(){save();onChange?.();}
    function render(){
      title.textContent=t('批注')+' ('+annotations.length+')';panel.hidden=!annotations.length;
      list.replaceChildren();inputs.clear();
      for(const item of annotations){
        const row=document.createElement('div');row.className='plan-annotation';row.dataset.annotation=item.id;
        const head=document.createElement('div');head.className='plan-annotation-head';
        const label=document.createElement('span');label.textContent=item.id+' · '+t(TYPES[item.type]);
        const remove=document.createElement('button');remove.type='button';remove.className='secondary plan-annotation-remove';remove.textContent=t('移除');
        remove.onclick=()=>{annotations=annotations.filter(entry=>entry!==item);render();changed();};
        head.append(label,remove);
        const quote=document.createElement('blockquote');quote.className='plan-annotation-quote';quote.textContent=item.quote;
        const input=document.createElement('textarea');input.rows=2;input.maxLength=MAX_BODY;input.value=item.body;
        input.placeholder=t(item.type==='delete'?'删除原因（可选）':'写下对这段的修改意见');
        input.setAttribute('aria-label',item.id+' '+t(TYPES[item.type]));
        input.oninput=()=>{item.body=input.value;changed();};
        row.append(head,quote,input);list.append(row);inputs.set(item.id,input);
      }
    }
    function add(type,picked){
      const next=annotations.reduce((max,item)=>Math.max(max,Number(item.id.slice(1))),0)+1;
      if(annotations.length>=MAX_ANNOTATIONS||next>999){notify?.(t('批注数量已达上限'));return;}
      const item={id:'A'+next,type,quote:picked.text,body:''};
      annotations.push(item);render();changed();
      if(type==='comment')inputs.get(item.id)?.focus?.();
      if(picked.truncated)notify?.(t('选中的内容较长，只引用了前面一部分'));
    }
    const onText=()=>save();
    textarea.addEventListener?.('input',onText);
    const controller=typeof BridgeSelection==='undefined'?null:BridgeSelection.attach({root,textarea,notify,messageOnly:false,
      actions:[{key:'comment',label:'批注'},{key:'delete',label:'建议删除'}],onPick:(picked,key)=>add(key,picked)});
    const current=()=>annotations.map(item=>({id:item.id,type:item.type,quote:item.quote,body:item.body}));
    const format=text=>formatFeedback(text,params,annotations);
    const payload=text=>({text:String(text??''),annotations:current()});
    function validate(text){
      if(!String(text??'').trim()&&!annotations.length){textarea.focus?.();return t('请填写修改意见');}
      const missing=annotations.find(item=>item.type==='comment'&&!item.body.trim());
      if(missing){inputs.get(missing.id)?.focus?.();return t('请为批注填写修改意见')+'：'+missing.id;}
      if(utf8Length(JSON.stringify({text:format(text),planFeedback:payload(text)}))>REQUEST_BUDGET)return t('批注内容过长，请精简后再提交');
      return '';
    }
    function clear(){annotations=[];render();try{storage?.removeItem(storageKey);}catch{}}
    function destroy(){controller?.destroy?.();textarea.removeEventListener?.('input',onText);}
    render();
    return {element:panel,count:()=>annotations.length,validate,format,payload,clear,destroy,add};
  }
  // 同一张卡片的历史与上一版批注共用一次只读查询；失败后允许重试。
  function memo(load){
    let pending=null;
    return ()=>pending||(pending=Promise.resolve().then(load).catch(error=>{pending=null;throw error;}));
  }
  function cleanFeedback(list){
    return Array.isArray(list)?list.filter(entry=>entry&&typeof entry==='object').map(entry=>({text:typeof entry.text==='string'?entry.text:'',annotations:cleanAnnotations(entry.annotations)})).filter(entry=>entry.text.trim()||entry.annotations.length):[];
  }
  // anchorText 存在时逐条标注锚点状态；它只是提示，不代表 Agent 已处理。
  function feedbackList(feedback,anchorText){
    const wrap=document.createElement('div');wrap.className='plan-feedback-list';
    for(const entry of feedback){
      for(const item of entry.annotations){
        const row=document.createElement('div');row.className='plan-annotation';row.dataset.annotation=item.id;
        const head=document.createElement('div');head.className='plan-annotation-head';
        const label=document.createElement('span');label.textContent=item.id+' · '+t(TYPES[item.type]);head.append(label);
        if(typeof anchorText==='string'){
          const found=anchorFound(item.quote,anchorText),status=document.createElement('span');
          status.className='plan-anchor-status '+(found?'present':'stale');
          status.textContent=t(found?'原文仍在，请确认是否已处理':'原文已修改或移除');head.append(status);
        }
        const quote=document.createElement('blockquote');quote.className='plan-annotation-quote';quote.textContent=item.quote;
        row.append(head,quote);
        if(item.body.trim()){const body=document.createElement('p');body.className='plan-annotation-body';body.textContent=item.body;row.append(body);}
        wrap.append(row);
      }
      if(entry.text.trim()){const global=document.createElement('p');global.className='plan-feedback-global';global.textContent=t('全局意见')+'：'+entry.text;wrap.append(global);}
    }
    return wrap;
  }
  function previousFeedbackDetails(params,load,currentText){
    const meta=metadata(params);
    if(!meta||meta.version<2||typeof load!=='function')return null;
    const details=document.createElement('details');details.className='plan-previous-feedback';
    const summary=document.createElement('summary');summary.textContent=t('上一版批注');
    const status=document.createElement('p');status.className='muted';
    details.append(summary,status);
    let loading=false,loaded=false;
    details.ontoggle=async()=>{
      if(!details.open||loading||loaded)return;
      loading=true;status.className='muted';status.textContent=t('正在加载历史版本…');
      try{
        const result=await load();
        if(!result||result.planId!==meta.id||!Array.isArray(result.revisions))throw Error(t('历史版本数据无效'));
        const previous=result.revisions.filter(revision=>Number.isSafeInteger(revision?.version)&&revision.version<meta.version).sort((a,b)=>b.version-a.version)[0];
        const feedback=cleanFeedback(previous?.feedback);
        if(!feedback.length){status.textContent=t('上一版没有记录批注');loaded=true;return;}
        status.textContent=t('版本')+' '+previous.version;
        details.append(feedbackList(feedback,typeof currentText==='function'?String(currentText()??''):String(params.planContent??'')));
        loaded=true;
      }catch(error){status.className='error';status.textContent=t('历史版本加载失败，请收起后重试')+'：'+error.message;}
      finally{loading=false;}
    };
    return details;
  }
  return {metadata,formatFeedback,versionBadge,diffDetails,foldRows,historyDetails,previousFeedbackDetails,reviewer,memo,anchorFound};
})();
if(typeof window!=='undefined')window.BridgePlanReview=BridgePlanReview;
if(typeof module!=='undefined')module.exports=BridgePlanReview;
