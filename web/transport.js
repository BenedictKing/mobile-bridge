'use strict';
// One live-update channel per chat. SSE carries ready/changes/reset frames
// when the gateway advertises it; tunnel hosts keep the changes long-poll.
// Both map onto the same callbacks so the timeline never knows the channel.
// Declared with var so manual browser checks can stub it from the console.
var BridgeTransport={
  // Capped doubling; only an application-level ready/success clears it.
  backoff(failures){return Math.min(1000<<Math.min(failures-1,8),32000);},
  // Active wake handles. A foreground return resyncs every open channel now
  // instead of waiting out a backoff ladder or a silently stalled socket.
  channels:new Set(),
  wake(){for(const wake of [...BridgeTransport.channels])wake();},
  subscribe(url,{onChanges,onReset,onDelta,onStatus,cursor}){
    let closed=false,failures=0,source=null,timer=null,polling=null,resume=null,lastId='',connecting=false,pacing=false,pollSession=0;
    function stop(){
      closed=true;BridgeTransport.channels.delete(wake);
      if(source)source.close();
      source=null;
      if(polling){polling.abort();polling=null;}
      if(timer!==null)clearTimeout(timer);
      timer=null;
    }
    function unauthorized(){if(BridgeConnection.active){BridgeConnection.disconnected();retry();return;}stop();onStatus?.('unauthorized');}
    function retry(){
      if(closed)return;
      onStatus?.('reconnecting');
      timer=setTimeout(()=>{timer=null;resume?resume():start();},BridgeTransport.backoff(++failures));
    }
    function wake(){
      // Foreground return: a stalled SSE socket is replaced and a pending
      // backoff fires now. A healthy long poll needs no push — its next
      // cycle is 200ms away and an in-flight request lands on its own.
      if(closed||connecting||pacing||polling)return;
      if(source){source.close();source=null;}
      if(timer!==null){clearTimeout(timer);timer=null;}
      failures=0;
      resume?resume():start();
    }
    // Only a definite auth loss logs out; network errors always reconnect.
    async function probe(){
      try{
        const response=await BridgeConnection.request('/api/auth',{credentials:'same-origin',cache:'no-store'});
        const info=response.ok?await response.json():null;
        if(closed)return;
        if(!info||info.authenticated===false)unauthorized();else retry();
      }catch{retry();}
    }
    async function start(){
      if(closed||connecting)return;
      connecting=true;
      try{
        const response=await BridgeConnection.request('/api/auth',{credentials:'same-origin',cache:'no-store'});
        const info=response.ok?await response.json():null;
        if(closed)return;
        if(!info||info.authenticated===false)return unauthorized();
        if(info.transport==='sse'&&typeof EventSource!=='undefined')return subscribeSse();
        subscribePoll();
      }catch{retry();}
      finally{connecting=false;}
    }
    function sseHandler(kind){
      return message=>{
        lastId=message.lastEventId||lastId;
        try{
          const page=JSON.parse(message.data);
          // delta frames patch rows in place (timeline.applyDelta); a frame
          // the timeline cannot apply falls back to a changes re-read there.
          if(kind==='delta')onDelta?.(page);
          else (kind==='reset'?onReset:onChanges)?.(page);
        }
        catch{/* A malformed frame never reaches apply; the next frame re-syncs. */}
      };
    }
    function subscribeSse(){
      resume=subscribeSse;
      // Wake and a late retry timer can race; always replace the old socket.
      if(source){source.close();source=null;}
      // Reconnects are managed here, not by EventSource, so the backoff and
      // the resume cursor stay ours; ?since= replays what the browser would
      // have sent in Last-Event-ID on its own retry.
      const current=cursor?.()||{};
      const since=lastId||(current.epoch&&current.after>=0?current.epoch+'.'+current.after:'');
      const target=url('events')+(url('events').includes('?')?'&':'?')+'incremental=1'
        +(since?'&since='+encodeURIComponent(since):'');
      source=BridgeConnection.eventSource(target);
      source.addEventListener('ready',()=>{failures=0;});
      source.addEventListener('changes',sseHandler('changes'));
      source.addEventListener('delta',sseHandler('delta'));
      source.addEventListener('reset',sseHandler('reset'));
      source.addEventListener('logout',unauthorized);
      source.onerror=()=>{if(closed||!source)return;source.close();source=null;probe();};
    }
    async function subscribePoll(){
      resume=subscribePoll;
      // A second entry (wake racing a retry) must retire the older loop.
      const session=++pollSession;
      while(!closed&&session===pollSession){
        let page=null;
        try{
          // The cursor is re-read every cycle: history loads may have moved
          // the window start while the previous long-poll was in flight.
          const state=cursor?.()||{};
          const target=url('changes')+(url('changes').includes('?')?'&':'?')
            +new URLSearchParams({after:state.after??-1,epoch:state.epoch||'',start:state.start||''});
          polling=new AbortController();
          const response=await BridgeConnection.request(target,{credentials:'same-origin',cache:'no-store',signal:polling.signal});
          if(closed)return;
          if(response.status===401)return unauthorized();
          if(!response.ok)throw Error('HTTP '+response.status);
          page=await response.json();
          failures=0;
        }catch(error){
          if(closed||error?.name==='AbortError')return;
          return probe();
        }
        (page&&page.reset?onReset:onChanges)?.(page);
        pacing=true;
        await new Promise(resolve=>{timer=setTimeout(resolve,200);});
        pacing=false;timer=null;
      }
    }
    start();
    BridgeTransport.channels.add(wake);
    return stop;
  }
};
