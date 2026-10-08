/* Cross-agent session migration entry, rendered inside the chat details dialog. */
class MigrateControl {
  constructor({root,request,confirm=()=>true,onMigrated=()=>{},onError=()=>{}}){
    Object.assign(this,{root,request,confirm,onMigrated,onError});
    this.view=null;this.busy=false;
  }
  text(value){return typeof BridgeI18n==='undefined'?value:BridgeI18n.t(value);}
  locale(){return typeof BridgeI18n==='undefined'?'zh-CN':BridgeI18n.locale();}
  label(kind){return typeof BridgeI18n==='undefined'?kind:BridgeI18n.agentLabel(kind);}
  node(tag,text,className){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(className)n.className=className;return n;}
  targets(){return (this.view?.agents||[]).filter(agent=>agent&&agent.kind&&agent.kind!==this.view?.agent);}
  render(view){
    if(view)this.view=view;
    if(this.busy)return;  // A confirm/POST cycle owns the controls.
    this.root.replaceChildren();
    const targets=this.targets();
    this.root.hidden=!targets.length;
    if(!targets.length)return;
    this.root.append(this.node('p',this.text('迁移到其他 Agent'),'migrate-heading'));
    const row=this.node('div',undefined,'migrate-targets');
    for(const agent of targets){
      const button=this.node('button',this.text('迁移到 ')+(agent.label||agent.kind));
      button.type='button';button.className='plain migrate-target';button.dataset.agent=agent.kind;
      button.onclick=()=>this.migrate(agent.kind);
      row.append(button);
    }
    this.root.append(row);
  }
  async migrate(kind){
    if(this.busy||!this.view)return;
    const label=this.label(kind);
    if(!this.confirm(this.text('将创建指向同一目录的 ')+label+this.text(' 新会话，并把标题与最近消息转录作为首条消息注入；源会话保持不变。')))return;
    this.busy=true;
    const buttons=[...this.root.querySelectorAll('button')];buttons.forEach(b=>b.disabled=true);
    try{
      const body={agent:kind,id:(typeof crypto.randomUUID==='function'?crypto.randomUUID():String(Date.now())),uiLocale:this.locale()};
      const result=await this.request(body);
      this.onMigrated(result,label);
    }catch(error){
      this.onError(error);
    }finally{
      this.busy=false;buttons.forEach(b=>b.disabled=false);
    }
  }
}
