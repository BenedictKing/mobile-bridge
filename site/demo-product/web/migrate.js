/* Cross-agent session migration entry, rendered inside the chat details dialog. */
const MIGRATE_SCOPES=[
  {key:'full',label:'完全复制历史'},
  {key:'main',label:'只复制主要历史'},
  {key:'latest',label:'只复制最新提问和结果'}];
const MIGRATE_SCOPE_COPY={
  full:' 新会话，并把完整历史转录作为首条消息注入；源会话保持不变。',
  main:' 新会话，并把标题与最近消息转录作为首条消息注入；源会话保持不变。',
  latest:' 新会话，并把最新提问与回答作为首条消息注入；源会话保持不变。'};
class MigrateControl {
  constructor({root,request,confirm=()=>true,onMigrated=()=>{},onError=()=>{}}){
    Object.assign(this,{root,request,confirm,onMigrated,onError});
    this.view=null;this.busy=false;this.picked=null;
  }
  text(value){return typeof BridgeI18n==='undefined'?value:BridgeI18n.t(value);}
  locale(){return typeof BridgeI18n==='undefined'?'zh-CN':BridgeI18n.locale();}
  label(kind){return typeof BridgeI18n==='undefined'?kind:BridgeI18n.agentLabel(kind);}
  node(tag,text,className){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(className)n.className=className;return n;}
  targets(){return (this.view?.agents||[]).filter(agent=>agent&&agent.kind&&agent.kind!==this.view?.agent);}
  render(view){
    if(view){
      if(this.view&&view.id!==this.view.id)this.picked=null;  // Another chat's details.
      this.view=view;
    }
    if(this.busy)return;  // A confirm/POST cycle owns the controls.
    this.root.replaceChildren();
    const targets=this.targets();
    this.root.hidden=!targets.length;
    if(!targets.length){this.picked=null;return;}
    // State updates re-render while picking; keep the chosen target valid.
    if(this.picked&&!targets.some(agent=>agent.kind===this.picked))this.picked=null;
    if(this.picked){
      this.renderScopes();
    }else{
      this.renderTargets(targets);
    }
  }
  renderTargets(targets){
    this.root.append(this.node('p',this.text('迁移到其他 Agent'),'migrate-heading'));
    const row=this.node('div',undefined,'migrate-targets');
    for(const agent of targets){
      const button=this.node('button',this.text('迁移到 ')+(agent.label||agent.kind));
      button.type='button';button.className='plain migrate-target';button.dataset.agent=agent.kind;
      button.onclick=()=>{this.picked=agent.kind;this.render();};
      row.append(button);
    }
    this.root.append(row);
  }
  renderScopes(){
    this.root.append(this.node('p',this.text('选择复制范围'),'migrate-heading'));
    const row=this.node('div',undefined,'migrate-scopes');
    for(const scope of MIGRATE_SCOPES){
      const button=this.node('button',this.text(scope.label));
      button.type='button';button.className='plain migrate-scope';button.dataset.scope=scope.key;
      button.onclick=()=>this.migrate(this.picked,scope.key);
      row.append(button);
    }
    const back=this.node('button',this.text('返回'));
    back.type='button';back.className='plain migrate-back';
    back.onclick=()=>{this.picked=null;this.render();};
    row.append(back);
    this.root.append(row);
  }
  async migrate(kind,scope='main'){
    if(this.busy||!this.view)return;
    const label=this.label(kind);
    if(!this.confirm(this.text('将创建指向同一目录的 ')+label+this.text(MIGRATE_SCOPE_COPY[scope]||MIGRATE_SCOPE_COPY.main)))return;
    this.busy=true;
    const buttons=[...this.root.querySelectorAll('button')];buttons.forEach(b=>b.disabled=true);
    try{
      const body={agent:kind,scope,id:(typeof crypto.randomUUID==='function'?crypto.randomUUID():String(Date.now())),uiLocale:this.locale()};
      const result=await this.request(body);
      this.onMigrated(result,label);
    }catch(error){
      this.onError(error);
    }finally{
      this.busy=false;
      this.render();
    }
  }
}
