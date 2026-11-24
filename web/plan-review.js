'use strict';
const BridgePlanReview=(()=>{
  const t=source=>typeof BridgeI18n==='undefined'?source:BridgeI18n.t(source);
  function metadata(params={}){
    const version=Number(params.planVersion);
    if(typeof params.planId!=='string'||!params.planId||!Number.isSafeInteger(version)||version<1||typeof params.planHash!=='string'||!/^[a-f0-9]{64}$/i.test(params.planHash))return null;
    return {id:params.planId,version,hash:params.planHash};
  }
  function formatFeedback(text,params){
    const meta=metadata(params);
    if(!meta)return text;
    return `## Plan revision feedback\n\nPlan-ID: ${JSON.stringify(meta.id)}\nPlan-Version: ${meta.version}\nPlan-SHA256: ${meta.hash}\n\nRevise the plan to address ALL feedback below before resubmitting. Do not resubmit the plan unchanged.\n\n${text}`;
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
          };
          list.append(item);
        }
        details.append(list);loaded=true;
      }catch(error){status.className='error';status.textContent=t('历史版本加载失败，请收起后重试')+'：'+error.message;}
      finally{loading=false;}
    };
    return details;
  }
  function attachSelection(root,textarea){
    if(!root||!textarea||typeof BridgeSelection==='undefined')return null;
    const controller=BridgeSelection.attach({root,textarea,messageOnly:false});
    return ()=>controller?.destroy?.();
  }
  return {metadata,formatFeedback,versionBadge,diffDetails,foldRows,historyDetails,attachSelection};
})();
if(typeof window!=='undefined')window.BridgePlanReview=BridgePlanReview;
if(typeof module!=='undefined')module.exports=BridgePlanReview;
