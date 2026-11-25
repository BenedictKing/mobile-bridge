'use strict';
// @ts-check
const BridgeLazy=(()=>{
  const groups={
    accounts:{scripts:['/account.js','/accounts.js','/pool.js']},
    markdown:{
      styles:['/vendor/katex/katex.min.css'],
      scripts:['/vendor/markdown-it.min.js','/vendor/katex/katex.min.js','/vendor/texmath.js'],
      after:['/diff.js','/markdown.js'],
    },
    graph:{scripts:['/vendor/dagre.min.js']},
    teamCanvas:{scripts:['/vendor/dagre.min.js','/team-canvas.js']},
    swarm:{scripts:['/team-swarm.js']},
  };
  function createLazyLoader(doc){
    const resources=new Map(),loading=new Map();
    let assets=null;
    function asset(path){
      if(!assets){
        const manifest=doc.getElementById('bridge-assets');
        assets=manifest?JSON.parse(manifest.textContent||'{}'):{};
      }
      return assets[path]||path;
    }
    function loadElement(path,kind){
      const url=asset(path),key=kind+':'+url;
      if(resources.has(key))return resources.get(key);
      const promise=new Promise((resolve,reject)=>{
        const node=doc.createElement(kind==='script'?'script':'link');
        if(kind==='script'){node.src=url;node.async=false;}
        else{node.rel='stylesheet';node.href=url;}
        node.dataset.bridgeLazy=path;
        node.onload=()=>resolve();
        node.onerror=()=>{node.remove();reject(Error('资源加载失败：'+path));};
        doc.head.append(node);
      });
      resources.set(key,promise);
      promise.catch(()=>resources.delete(key));
      return promise;
    }
    async function loadScripts(paths=[]){for(const path of paths)await loadElement(path,'script');}
    function load(name){
      if(loading.has(name))return loading.get(name);
      const group=groups[name];
      if(!group)return Promise.reject(Error('未知延迟资源组：'+name));
      const promise=(async()=>{
        await Promise.all([
          Promise.all((group.styles||[]).map(path=>loadElement(path,'style'))),
          loadScripts(group.scripts),
        ]);
        await loadScripts(group.after);
      })();
      loading.set(name,promise);
      promise.catch(()=>loading.delete(name));
      return promise;
    }
    return {load};
  }
  if(typeof module!=='undefined')module.exports={createLazyLoader,groups};
  return typeof document==='undefined'?null:createLazyLoader(document);
})();
