'use strict';
(() => {
  const markdown = window.markdownit({html:false, linkify:true, breaks:true});
  // Math delimiters must be consumed before Markdown removes backslash escapes.
  markdown.use(texmath, {engine:window.katex, delimiters:['brackets','dollars']});
  const tags = new Set(['p','h1','h2','h3','h4','h5','h6','ul','ol','li','strong','em','s','blockquote','hr','br','table','thead','tbody','tr','th','td']);

  // Build nodes from parsed tokens; model output never becomes executable HTML.
  window.renderMarkdown = function(node, text, files = [], fileUrl = () => '', options = {}) {
    const t = x => typeof BridgeI18n === 'undefined' ? x : BridgeI18n.t(x);
    const body = document.createElement('div');
    body.className = 'markdown-body';
    const artifacts = new Map(files.map(file => [markdown.normalizeLink(file.reference), file]));
    const htmlArtifact = file => /\.html?$/i.test(String(file?.reference || ''));
    function target(reference) {
      const file = artifacts.get(reference);
      if (file) return {url:fileUrl(file), file};
      if (/^https?:\/\//i.test(reference)) return {url:reference};
      return null;
    }
    function link(url, title) {
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.target = '_blank';
      anchor.rel = 'noopener noreferrer';
      if (title) anchor.title = title;
      return anchor;
    }
    // Collapsed in-chat preview for .html/.htm artifacts: a sandboxed iframe
    // that only fetches its src when expanded, plus a keep-new-tab link.
    function htmlPreview(file, url) {
      const name = file.name || file.reference;
      const details = document.createElement('details');
      details.className = 'artifact-preview';
      const summary = document.createElement('summary');
      summary.append(t('预览') + ' ' + name + ' · ');
      const opener = link(url, null);
      opener.textContent = t('在新标签打开');
      summary.append(opener);
      const frame = document.createElement('iframe');
      frame.className = 'artifact-frame';
      frame.title = name;
      frame.setAttribute('sandbox', 'allow-scripts');
      frame.setAttribute('loading', 'lazy');
      frame.setAttribute('referrerpolicy', 'no-referrer');
      details.ontoggle = () => { if (details.open && !frame.src) frame.src = url; };
      details.append(summary, frame);
      return details;
    }
    let codeIndex=0;
    function appendTokens(tokens, root) {
      const parents = [root];
      for (const token of tokens) {
        if (token.hidden) continue;
        const parent = parents[parents.length - 1];
        if (token.type === 'inline') {
          appendTokens(token.children || [], parent);
        } else if (token.type.startsWith('math_')) {
          const display = token.block || token.type === 'math_inline_double';
          const formula = document.createElement(token.block ? 'div' : 'span');
          formula.className = display ? 'math-display' : 'math-inline';
          try {
            window.katex.render(token.content, formula, {
              displayMode:display, throwOnError:true, trust:false,
              maxExpand:1000, maxSize:20, strict:'ignore'
            });
          } catch {
            // Incomplete or unsupported TeX stays readable during streaming.
            const source = document.createElement('code');
            source.textContent = token.content;
            formula.append(source);
          }
          if (token.type.endsWith('_eqno')) {
            const number = document.createElement('span');
            number.className = 'math-number';
            number.textContent = '(' + token.info + ')';
            formula.append(number);
          }
          parent.append(formula);
        } else if (token.type === 'text') {
          parent.append(document.createTextNode(token.content));
        } else if (token.type === 'softbreak' || token.type === 'hardbreak') {
          parent.append(document.createElement('br'));
        } else if (token.type === 'code_inline' || token.type === 'code_block' || token.type === 'fence') {
          const code = document.createElement('code');
          code.textContent = token.content;
          if (token.type === 'code_inline') parent.append(code);
          else {
            const pre = document.createElement('pre');
            pre.append(code);
            const wrapper=document.createElement('div');wrapper.className='code-block';
            const button=document.createElement('button');button.className='plain code-copy';button.type='button';button.textContent=typeof BridgeI18n==='undefined'?'复制代码':BridgeI18n.t('复制代码');
            const index=codeIndex++;
            button.onclick=()=>window.BridgeClipboard?.copy(async()=>{
              if(!options.fullText)return token.content;
              const complete=await options.fullText();
              const blocks=markdown.parse(complete,{}).filter(t=>t.type==='fence'||t.type==='code_block');
              if(!blocks[index])throw Error('读取完整正文失败，请重试');
              return blocks[index].content;
            },button);
            wrapper.append(button,pre);parent.append(wrapper);
          }
        } else if (token.type === 'image') {
          const destination = target(token.attrGet('src') || '');
          const label = token.content || destination?.file?.name || '图片';
          if (destination?.file && htmlArtifact(destination.file)) {
            parent.append(htmlPreview(destination.file, destination.url));
          } else if (destination?.file?.image) {
            // The lightbox opens the image in place; the anchor keeps the
            // full-size new-tab entry for long-press / modifier clicks.
            const imageUrl = BridgeConnection.url(destination.url);
            const anchor = link(imageUrl, token.attrGet('title'));
            anchor.dataset.imagePreview = imageUrl;
            anchor.dataset.imageName = label;
            const image = document.createElement('img');
            image.className = 'artifact-image';
            image.src = imageUrl;
            image.alt = label;
            image.loading = 'lazy';
            anchor.append(image);
            parent.append(anchor);
          } else if (destination) {
            // Remote images remain links instead of making background requests.
            const anchor = link(destination.url, token.attrGet('title'));
            anchor.textContent = label;
            parent.append(anchor);
          } else parent.append(document.createTextNode(label));
        } else if (token.nesting === -1) {
          const closed = parents.pop();
          // An inline link to an HTML artifact gains its preview right after
          // the anchor (the anchor is the parent's last child here).
          if (closed?.dataset?.artifactPreview && closed.parentNode) {
            closed.parentNode.append(htmlPreview(closed.previewFile, closed.previewUrl));
          }
        } else {
          let element;
          if (token.type === 'link_open') {
            const destination = target(token.attrGet('href') || '');
            if (destination?.file && htmlArtifact(destination.file)) {
              element = link(destination.url, token.attrGet('title'));
              element.dataset.artifactPreview = '1';
              element.previewFile = destination.file;
              element.previewUrl = destination.url;
            } else {
              element = destination ? link(destination.url, token.attrGet('title')) : document.createElement('span');
              if (destination?.file) element.download = destination.file.name || '';
            }
          } else if (tags.has(token.tag)) {
            element = document.createElement(token.tag);
            if (token.tag === 'ol' && token.attrGet('start')) element.start = Number(token.attrGet('start'));
            const alignment = /^text-align:(left|center|right)$/.exec(token.attrGet('style') || '');
            if (alignment) element.className = 'align-' + alignment[1];
          } else continue;
          if (token.tag === 'table') {
            const scroll = document.createElement('div');
            scroll.className = 'markdown-table';
            scroll.tabIndex = 0;
            scroll.setAttribute('role', 'region');
            scroll.setAttribute('aria-label', '表格，可横向滚动');
            scroll.append(element);
            parent.append(scroll);
          } else parent.append(element);
          if (token.nesting === 1) parents.push(element);
        }
      }
    }
    appendTokens(markdown.parse(String(text || ''), {}), body);
    node.append(body);
  };
})();
