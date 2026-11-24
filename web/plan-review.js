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
  function diffDetails(params){
    const meta=metadata(params),previous=params.previousPlanContent,current=params.planContent;
    if(!meta||meta.version<2||typeof previous!=='string'||typeof current!=='string'||previous===current||!window.BridgeDiff?.pairRows)return null;
    const result=window.BridgeDiff.pairRows(previous,current),rows=result.rows;
    const additions=rows.filter(row=>row.kind==='add').length,deletions=rows.filter(row=>row.kind==='del').length;
    const details=document.createElement('details');details.className='plan-revision-diff';
    const summary=document.createElement('summary');summary.append(t('查看与上一版的差异')+' · ');
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
  function attachSelection(root,textarea){
    if(!root||!textarea||typeof BridgeSelection==='undefined')return null;
    const controller=BridgeSelection.attach({root,textarea,messageOnly:false});
    return ()=>controller?.destroy?.();
  }
  return {metadata,formatFeedback,versionBadge,diffDetails,foldRows,attachSelection};
})();
if(typeof window!=='undefined')window.BridgePlanReview=BridgePlanReview;
if(typeof module!=='undefined')module.exports=BridgePlanReview;
