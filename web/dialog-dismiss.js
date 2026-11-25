'use strict';
/* 点遮罩关闭弹窗。只给无需确认的弹窗用（查看、导航、即时生效或单步保存的设置）；
   有长文本编辑、创建或审批决策的弹窗不接入，仍只能按钮或 Esc 关闭。
   静态弹窗在 index.html 上加 data-backdrop-close 声明，动态弹窗调用 closeOnBackdrop。
   判定：backdrop 上的点击落在 dialog 自身，但 .picker 的内边距同样是 dialog，
   所以再按坐标排除面板矩形；按下也须在面板外，避免从面板里拖选文字到外面松手误关。
   多行文本框里打过字就不关，防止误触丢草稿；关闭后重置。 */
const BridgeDialog=(()=>{
  function outside(dialog,event){
    if(event.target!==dialog)return false;
    const box=dialog.getBoundingClientRect();
    return event.clientX<box.left||event.clientX>box.right||event.clientY<box.top||event.clientY>box.bottom;
  }
  function closeOnBackdrop(dialog){
    if(!dialog||dialog.dataset.backdropBound)return;
    dialog.dataset.backdropBound='1';
    let pressedOutside=false,typed=false;
    dialog.addEventListener('pointerdown',event=>{pressedOutside=outside(dialog,event);});
    dialog.addEventListener('input',event=>{if(event.target?.tagName==='TEXTAREA')typed=true;});
    dialog.addEventListener('close',()=>{typed=false;pressedOutside=false;});
    dialog.addEventListener('click',event=>{
      if(pressedOutside&&!typed&&outside(dialog,event))dialog.close();
      pressedOutside=false;
    });
  }
  function bindDeclared(root){root.querySelectorAll('dialog[data-backdrop-close]').forEach(closeOnBackdrop);}
  return {closeOnBackdrop,bindDeclared};
})();
if(typeof window!=='undefined'){
  window.BridgeDialog=BridgeDialog;
  if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',()=>BridgeDialog.bindDeclared(document));
  else BridgeDialog.bindDeclared(document);
}
if(typeof module!=='undefined')module.exports=BridgeDialog;
