'use strict';
(() => {
  const t = text => typeof BridgeI18n === 'undefined' ? text : BridgeI18n.t(text);
  // Only gateway-served images may open here: phone upload previews, desktop
  // images and chat file artifacts under /api/sessions/{id}/. Session ids are
  // gateway-opaque (uuid or native agent id); the sensitive tails stay strict.
  const whitelist = /^\/api\/sessions\/[^/?#]+\/(?:uploads\/[0-9a-f-]{36}\/preview|desktop-images\/[a-f0-9]{64}|files\/[a-f0-9]{64})$/;
  let dialog, title, content, status, close, external, opener, href = '';
  // Zoom/pan state. The image keeps its contain-fit layout box; a centered
  // translate+scale transform does the rest, so layout never reflows mid-gesture.
  const MAX_SCALE = 6, TAP_SLOP = 10, DOUBLE_TAP_MS = 300;
  let view = {scale: 1, x: 0, y: 0}, pointers = new Map(), gesture = null, lastTap = null, swallowClick = false;
  const imageOf = () => content?.querySelector('img');
  function apply(image = imageOf()) {
    content?.classList.toggle('zoomed', view.scale > 1);
    if (image) image.style.transform = view.scale === 1 ? '' : `translate(${view.x}px, ${view.y}px) scale(${view.scale})`;
  }
  function reset() { view = {scale: 1, x: 0, y: 0}; pointers.clear(); gesture = null; lastTap = null; apply(); }
  // Layout box of the untransformed image: transformed rect minus the current view.
  function layout(image) {
    const rect = image.getBoundingClientRect();
    return {cx: rect.left + rect.width / 2 - view.x, cy: rect.top + rect.height / 2 - view.y, w: rect.width / view.scale, h: rect.height / view.scale};
  }
  // Keep the zoomed image covering its original box so it can't be flung away.
  function clamp(next, box) {
    const scale = Math.min(MAX_SCALE, Math.max(1, next.scale));
    const mx = box.w * (scale - 1) / 2, my = box.h * (scale - 1) / 2;
    return {scale, x: Math.min(mx, Math.max(-mx, next.x)), y: Math.min(my, Math.max(-my, next.y))};
  }
  // Scale about a screen point (px,py): that point stays under the finger/cursor.
  function zoomAround(scale, px, py, box, from = view) {
    const k = Math.min(MAX_SCALE, Math.max(1, scale)) / from.scale;
    return {scale: from.scale * k, x: px - box.cx - (px - box.cx - from.x) * k, y: py - box.cy - (py - box.cy - from.y) * k};
  }
  function beginGesture(image) {
    const box = layout(image), points = [...pointers.values()];
    if (points.length >= 2) {
      const [a, b] = points;
      gesture = {kind: 'pinch', box, from: {...view}, mid: {x: (a.x + b.x) / 2, y: (a.y + b.y) / 2}, distance: Math.max(1, Math.hypot(a.x - b.x, a.y - b.y))};
    } else if (points.length === 1) {
      gesture = {kind: 'pan', box, from: {...view}, start: {...points[0]}};
    } else gesture = null;
  }
  function onPointerDown(event) {
    const image = imageOf();
    if (!image || image.hidden || (event.pointerType === 'mouse' && event.button !== 0)) return;
    if (!pointers.size) swallowClick = false;
    pointers.set(event.pointerId, {x: event.clientX, y: event.clientY, t: event.timeStamp, sx: event.clientX, sy: event.clientY, moved: false});
    beginGesture(image);
  }
  function onPointerMove(event) {
    const point = pointers.get(event.pointerId), image = imageOf();
    if (!point || !gesture || !image) return;
    point.x = event.clientX; point.y = event.clientY;
    if (Math.hypot(point.x - point.sx, point.y - point.sy) > TAP_SLOP) point.moved = true;
    if (gesture.kind === 'pinch') {
      const [a, b] = [...pointers.values()], mid = {x: (a.x + b.x) / 2, y: (a.y + b.y) / 2};
      const scale = gesture.from.scale * Math.hypot(a.x - b.x, a.y - b.y) / gesture.distance;
      const anchored = zoomAround(scale, gesture.mid.x, gesture.mid.y, gesture.box, gesture.from);
      view = clamp({...anchored, x: anchored.x + mid.x - gesture.mid.x, y: anchored.y + mid.y - gesture.mid.y}, gesture.box);
    } else if (gesture.from.scale > 1) {
      view = clamp({...gesture.from, x: gesture.from.x + point.x - gesture.start.x, y: gesture.from.y + point.y - gesture.start.y}, gesture.box);
    } else return;
    event.preventDefault?.(); apply(image);
  }
  function onPointerUp(event) {
    const point = pointers.get(event.pointerId), image = imageOf();
    if (!point) return;
    const wasPinch = gesture?.kind === 'pinch';
    pointers.delete(event.pointerId);
    // A drag or pinch that lifts over empty space must not count as a dismiss click.
    if (point.moved || wasPinch) swallowClick = true;
    if (image) beginGesture(image);
    if (point.moved || wasPinch || event.type === 'pointercancel' || !image || event.target !== image) { lastTap = null; return; }
    const tap = {x: event.clientX, y: event.clientY, t: event.timeStamp};
    if (lastTap && tap.t - lastTap.t < DOUBLE_TAP_MS && Math.hypot(tap.x - lastTap.x, tap.y - lastTap.y) < TAP_SLOP * 3) {
      const box = layout(image);
      view = view.scale > 1 ? {scale: 1, x: 0, y: 0} : clamp(zoomAround(2.5, tap.x, tap.y, box), box);
      apply(image); lastTap = null;
    } else lastTap = tap;
  }
  function onWheel(event) {
    const image = imageOf();
    if (!image || image.hidden) return;
    event.preventDefault();
    const box = layout(image);
    view = clamp(zoomAround(view.scale * Math.exp(-event.deltaY * (event.ctrlKey ? 0.01 : 0.002)), event.clientX, event.clientY, box), box);
    apply(image);
  }
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
      content.replaceChildren(); status.textContent = ''; href = ''; reset();
      if (opener?.isConnected) opener.focus({preventScroll:true});
      opener = null;
    });
    // The full-screen dialog covers its native backdrop, so empty space lands
    // on the content container. Only the image and toolbar stay non-dismissive.
    dialog.addEventListener('click', event => {
      if (swallowClick) { swallowClick = false; return; }
      if (event.target === dialog || event.target === content) dialog.close();
    });
    // No pointer capture: touch already captures implicitly to the image, and an
    // explicit capture would retarget the follow-up click to content (= dismiss).
    content.addEventListener('pointerdown', onPointerDown);
    content.addEventListener('pointermove', onPointerMove);
    for (const kind of ['pointerup', 'pointercancel']) content.addEventListener(kind, onPointerUp);
    content.addEventListener('wheel', onWheel, {passive: false});
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
    const image = document.createElement('img'); image.alt = title.textContent; image.draggable = false;
    image.onload = () => { if (image.isConnected) { image.hidden = false; status.textContent = ''; } };
    image.onerror = () => { if (image.isConnected && !image.dataset.publicFallback) { image.hidden = true; status.textContent = t('图片加载失败，请关闭后重试。'); } };
    content.replaceChildren(image); reset(); image.src = url.href;
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
