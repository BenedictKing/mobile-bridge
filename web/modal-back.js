'use strict';
/* 返回手势关闭弹窗。界面的模态弹窗（<dialog>.showModal）原本不占历史栈，
   安卓返回手势 / iOS 侧滑 / 浏览器后退会直接退掉聊天页甚至整个 App。
   这里在弹窗由用户打开时压一条同 URL 的历史记录，返回时只关最上层弹窗；
   弹窗经按钮、Esc 或点背景关闭时再把这条记录退掉，历史栈保持原样。
   必须先于 app.js 加载：popstate 监听按注册顺序执行，关弹窗的那次返回要在
   这里截停，不能再落到 app.js 的路由分发（它会清 detailPushed、切换视图）。 */
(() => {
  const proto = typeof HTMLDialogElement === 'undefined' ? null : HTMLDialogElement.prototype;
  if (!proto?.showModal) return;
  const KEY = 'bridgeModal';
  // 每次加载一个前缀：刷新后历史栈里残留的旧记录不会与新编号撞上。
  const load = Math.random().toString(36).slice(2);
  let seq = 0, ignorePops = 0;
  const stack = []; // {dialog, id, popped}，与压入的历史记录一一对应
  const live = record => record.dialog.open && record.dialog.isConnected;
  function push(dialog) {
    const id = load + ':' + ++seq;
    history.pushState({...(history.state || {}), [KEY]: id}, '', location.href);
    stack.push({dialog, id, popped: false});
  }
  const showModal = proto.showModal;
  proto.showModal = function (...args) {
    const wasOpen = this.open;
    const result = showModal.apply(this, args);
    // 非用户触发（推送来的审批等）不压栈：Chrome 返回时会跳过这类记录，
    // 压了反而让弹窗关闭与页面后退同时发生。
    const activated = navigator.userActivation ? navigator.userActivation.isActive : true;
    if (!wasOpen && this.open && activated) push(this);
    return result;
  };
  // close 不冒泡，捕获阶段才能统一收到所有弹窗的关闭。
  document.addEventListener('close', event => {
    const index = stack.findIndex(record => record.dialog === event.target);
    if (index < 0) return;
    const [record] = stack.splice(index, 1);
    if (record.popped) return;
    // 只在本条记录仍在栈顶时退栈；关闭后已有路由压栈/替换（如从弹窗进入聊天），
    // 这时 back() 会误退新页面，留下的死记录在 popstate 里跳过。
    if (history.state?.[KEY] === record.id) { ignorePops++; history.back(); }
  }, true);
  window.addEventListener('popstate', event => {
    if (ignorePops) { ignorePops--; event.stopImmediatePropagation(); return; }
    while (stack.length && !live(stack[stack.length - 1])) stack.pop();
    const top = stack[stack.length - 1], here = history.state?.[KEY];
    if (top && here !== top.id) {
      event.stopImmediatePropagation();
      stack.pop(); top.popped = true;
      const cancel = new Event('cancel', {cancelable: true});
      top.dialog.dispatchEvent(cancel);
      // 弹窗忙碌时拒绝取消（如正在创建会话）：补回记录，保持一一对应。
      if (cancel.defaultPrevented) push(top.dialog);
      else top.dialog.close();
      return;
    }
    // 退到一条弹窗已不在的死记录：它与下一条同 URL，直接再退一步交给路由。
    if (here && !stack.some(record => record.id === here)) {
      event.stopImmediatePropagation();
      history.back();
    }
  });
})();
