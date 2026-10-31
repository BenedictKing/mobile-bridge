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
  constructor({root,request,confirm=()=>true,onMigrated=()=>{},onError=()=>{},onBusy=()=>{},enabled=()=>true}){
    Object.assign(this,{root,request,confirm,onMigrated,onError,onBusy,enabled});
    this.view=null;this.busy=false;this.picked=null;
    this.prepared=null;this.choice={model:'',effort:''};  // Settings step: {id,kind,scope,catalog}.
    this.epoch=0;this.confirming=false;
  }
  text(value){return typeof BridgeI18n==='undefined'?value:BridgeI18n.t(value);}
  locale(){return typeof BridgeI18n==='undefined'?'zh-CN':BridgeI18n.locale();}
  label(kind){return typeof BridgeI18n==='undefined'?kind:BridgeI18n.agentLabel(kind);}
  node(tag,text,className){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(className)n.className=className;return n;}
  targets(){return (this.view?.agents||[]).filter(agent=>agent&&agent.kind&&agent.kind!==this.view?.agent);}
  render(view){
    if(view){
      if(this.view&&view.id!==this.view.id){this.cancel();this.picked=null;}  // Another chat's details.
      this.view=view;
    }
    if(this.busy)return;  // A confirm/POST cycle owns the controls.
    this.root.replaceChildren();
    const targets=this.targets();
    this.root.hidden=!targets.length;
    if(!targets.length){this.cancel();this.picked=null;return;}
    // State updates re-render while picking; keep the chosen target valid.
    if(this.picked&&!targets.some(agent=>agent.kind===this.picked)){this.cancel();this.picked=null;}
    if(this.picked&&this.prepared){
      this.renderSettings();
    }else if(this.picked){
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
      button.type='button';button.className='plain migrate-target';button.dataset.agent=agent.kind;button.disabled=!this.enabled();
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
      button.type='button';button.className='plain migrate-scope';button.dataset.scope=scope.key;button.disabled=!this.enabled();
      button.onclick=()=>this.prepare(this.picked,scope.key);
      row.append(button);
    }
    const back=this.node('button',this.text('返回'));
    back.type='button';back.className='plain migrate-back';
    back.onclick=()=>{this.picked=null;this.render();};
    row.append(back);
    this.root.append(row);
  }
  body(kind,scope,id,extra){
    return {agent:kind,scope,id,uiLocale:this.locale(),...extra};
  }
  newId(){return typeof crypto.randomUUID==='function'?crypto.randomUUID():String(Date.now());}
  // Asks the gateway for the target's real model catalog. ACP agents only
  // advertise models once a session exists, so the gateway creates the
  // (still empty) target session here and the confirm step reuses it.
  async prepare(kind,scope){
    if(this.busy||!this.view||!this.enabled())return;
    this.busy=true;
    [...this.root.querySelectorAll('button')].forEach(b=>b.disabled=true);
    this.onBusy(true);
    const epoch=this.epoch,id=this.newId();
    try{
      const result=await this.request(this.body(kind,scope,id,{phase:'prepare'}));
      const prepared={id,kind,scope,catalog:result?.catalog||{}};
      if(epoch!==this.epoch)this.abandon(prepared);  // Dismissed while the target was being prepared.
      else{this.prepared=prepared;this.choice={model:'',effort:''};}
    }catch(error){
      if(epoch===this.epoch)this.onError(error);
    }finally{
      this.busy=false;
      this.render();
      this.onBusy(false);
    }
  }
  // Drops the empty target session a prepare created. Best effort: a failure
  // only leaves an empty chat in the target agent's list.
  abandon(prepared){
    Promise.resolve(this.request(this.body(prepared.kind,prepared.scope,prepared.id,{phase:'abandon'}))).catch(()=>{});
  }
  // Leaves the settings step. A running confirm owns the prepared session
  // (the gateway is configuring and prompting it), so it is never abandoned.
  cancel(){
    this.epoch++;
    const prepared=this.prepared;
    this.prepared=null;this.choice={model:'',effort:''};
    if(prepared&&!this.confirming)this.abandon(prepared);
  }
  back(){
    this.cancel();
    this.render();
  }
  // The details dialog was closed or replaced: forget any half-made choice.
  dismiss(){
    this.cancel();
    this.picked=null;
    this.render();
  }
  models(){return Array.isArray(this.prepared?.catalog?.models)?this.prepared.catalog.models:[];}
  efforts(model){
    const list=Array.isArray(model?.efforts)?model.efforts:[],catalog=this.prepared?.catalog||{};
    return {list,initial:[catalog.currentEffort,model?.defaultEffort].find(value=>value&&list.includes(value))||list[0]||''};
  }
  option(value,text){const n=document.createElement('option');n.value=value;n.textContent=text;return n;}
  field(label,select){
    const wrap=this.node('label',undefined,'migrate-field');
    wrap.append(this.node('span',this.text(label)),select);
    return wrap;
  }
  renderSettings(){
    const catalog=this.prepared.catalog,models=this.models();
    this.root.append(this.node('p',this.text('选择目标模型与思考强度'),'migrate-heading'));
    if(!models.length){
      this.root.append(this.node('p',this.text(catalog.catalogError?'无法读取目标模型列表，将使用目标 Agent 的默认设置。':'目标 Agent 没有可选模型，将使用其默认设置。'),'muted'));
    }else{
      const modelSelect=this.node('select',undefined,'migrate-model');
      modelSelect.append(this.option('',this.text('使用目标 Agent 的默认设置')));
      for(const model of models)modelSelect.append(this.option(model.id,model.name||model.id));
      const effortSelect=this.node('select',undefined,'migrate-effort');
      const paint=()=>{
        const model=models.find(item=>item.id===this.choice.model),{list,initial}=this.efforts(model);
        effortSelect.replaceChildren(...list.map(value=>this.option(value,value)));
        if(model&&!list.includes(this.choice.effort))this.choice.effort=initial;
        effortSelect.value=model?this.choice.effort:'';
        effortSelect.disabled=!model||!list.length;
      };
      modelSelect.value=this.choice.model;
      modelSelect.onchange=()=>{this.choice={model:modelSelect.value,effort:''};paint();};
      effortSelect.onchange=()=>{this.choice.effort=effortSelect.value;};
      paint();
      this.root.append(this.field('模型',modelSelect),this.field('思考强度',effortSelect));
    }
    const row=this.node('div',undefined,'migrate-scopes');
    const start=this.node('button',this.text('开始迁移'));
    start.type='button';start.className='primary migrate-start';start.disabled=!this.enabled();
    start.onclick=()=>this.migrate(this.picked,this.prepared.scope,'migrate',{id:this.prepared.id,model:this.choice.model,effort:this.choice.model?this.choice.effort:''});
    const back=this.node('button',this.text('返回'));
    back.type='button';back.className='plain migrate-back';
    back.onclick=()=>this.back();
    row.append(start,back);
    this.root.append(row);
  }
  async migrate(kind,scope='main',operation='migrate',options={}){
    if(this.busy||!this.view||!this.enabled())return;
    const label=this.label(kind);
    if(!this.confirm(this.text('将创建指向同一目录的 ')+label+this.text(MIGRATE_SCOPE_COPY[scope]||MIGRATE_SCOPE_COPY.main)))return;
    this.busy=true;this.confirming=!!options.id;
    const buttons=[...this.root.querySelectorAll('button')];buttons.forEach(b=>b.disabled=true);
    this.onBusy(true);
    try{
      const body=this.body(kind,scope,options.id||this.newId(),options.model?{model:options.model,effort:options.effort||''}:{});
      const result=await this.request(body);
      this.prepared=null;  // The target session is now a real migration.
      this.onMigrated(result,label,body,operation);
    }catch(error){
      this.onError(error,operation);
    }finally{
      this.busy=false;this.confirming=false;
      this.render();
      this.onBusy(false);
    }
  }
}
