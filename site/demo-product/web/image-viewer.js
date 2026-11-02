'use strict';
(() => {
  const t = text => typeof BridgeI18n === 'undefined' ? text : BridgeI18n.t(text);
  // Only gateway-served images may open here: phone upload previews, desktop
  // images and chat file artifacts under /api/sessions/{id}/. Session ids are
  // gateway-opaque (uuid or native agent id); the sensitive tails stay strict.
  const whitelist = /^\/api\/sessions\/[^/?#]+\/(?:uploads\/[0-9a-f-]{36}\/preview|desktop-images\/[a-f0-9]{64}|files\/[a-f0-9]{64})$/;
  let dialog, title, content, status, close, external, opener, href = '';
  function create() {
    dialog = document.createElement('dialog'); dialog.id = 'image-preview-dialog'; dialog.className = 'image-preview';
    dialog.setAttribute('aria-labelledby', 'image-preview-title');
    const toolbar = document.createElement('div'); toolbar.className = 'image-preview-toolbar';
    close = document.createElement('button'); close.id = 'image-preview-close'; close.type = 'button'; close.autofocus = true;
    close.onclick = () => dialog.close();
    title = document.createElement('span'); title.id = 'image-preview-title';
    external = document.createElement('button'); external.id = 'image-preview-external'; external.type = 'button';
    external.onclick = () => { if (href) window.open(href, '_blank', 'noopener'); };
    toolbar.append(close, title, external);
    content = document.createElement('div'); content.className = 'image-preview-content';
    status = document.createElement('p'); status.id = 'image-preview-status'; status.setAttribute('role', 'status');
    dialog.append(toolbar, content, status); document.body.append(dialog);
    dialog.addEventListener('close', () => {
      // A queued close event can arrive after another image has opened.
      if (dialog.open) return;
      content.replaceChildren(); status.textContent = ''; href = '';
      if (opener?.isConnected) opener.focus({preventScroll:true});
      opener = null;
    });
    // Tapping the backdrop (the dialog element itself) closes; Esc already
    // does via the native <dialog> behavior.
    dialog.addEventListener('click', event => { if (event.target === dialog) dialog.close(); });
  }
  function open(target, event) {
    const url = new URL(target.dataset.imagePreview, location.href);
    if (!BridgeConnection.trusted(url.href) || !whitelist.test(url.pathname)) return;
    event.preventDefault();
    if (!dialog) create();
    opener = target; href = url.href;
    close.textContent = '× ' + t('关闭'); close.setAttribute('aria-label', t('关闭图片预览'));
    external.textContent = t('在新标签打开'); external.setAttribute('aria-label', t('在新标签打开图片'));
    title.textContent = target.dataset.imageName || target.querySelector('img')?.alt || target.alt || t('图片附件');
    status.textContent = t('正在加载图片…');
    const image = document.createElement('img'); image.alt = title.textContent;
    image.onload = () => { if (image.isConnected) { image.hidden = false; status.textContent = ''; } };
    image.onerror = () => { if (image.isConnected && !image.dataset.publicFallback) { image.hidden = true; status.textContent = t('图片加载失败，请关闭后重试。'); } };
    content.replaceChildren(image); image.src = url.href;
    if (!dialog.open) dialog.showModal();
  }
  document.addEventListener('click', event => {
    const target = event.target.closest('[data-image-preview]');
    if (target && !event.defaultPrevented && event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) open(target, event);
  });
  document.addEventListener('keydown', event => {
    if (event.target.matches('img[data-image-preview]') && ['Enter', ' '].includes(event.key)) open(event.target, event);
  });
})();
