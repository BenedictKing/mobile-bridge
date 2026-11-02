// Synthetic presentation data only. No credentials or native account operations.
(()=>{
// 演示界面会随页面语言切换，账号名也要跟着走，否则英文界面里会夹中文。
const lang=()=>{try{const v=localStorage.getItem('bridge-language');if(v)return v==='en'?'en':'zh';}catch{}return document.documentElement.lang==='en'?'en':'zh';};
const T=(zh,en)=>lang()==='en'?en:zh;
const labels={official:['工作账号','Work account'],backup:['备用账号','Backup account'],api:['自建 API','Custom API']};
const localize=value=>{const copy=structuredClone(value);for(const row of copy.accounts||[]){if(labels[row.id])row.name=T(...labels[row.id]);}if(copy.current&&labels[copy.current.id])copy.current.name=T(...labels[copy.current.id]);for(const row of copy.discovery?.candidates||[]){if(row.id==='source-api')row.name=T('本机 API 配置','Local API config');}return copy;};
const usage=(a,b,n)=>({status:'ready',checkedAt:Date.now()/1000,limits:[{name:'Codex',windows:[{windowDurationMins:300,remainingPercent:a},{windowDurationMins:10080,remainingPercent:b}]}],resetCredits:{availableCount:n,credits:null}});
const data={accounts:[{id:'official',name:'工作账号',kind:'chatgpt',email:'work@example.com',planType:'Plus',details:{usage:usage(82,64,3)}},{id:'backup',name:'备用账号',kind:'chatgpt',email:'backup@example.com',planType:'Plus',details:{usage:usage(46,71,1)}},{id:'api',name:'自建 API',kind:'api',baseUrl:'https://api.example.com/v1',model:'gemini-2.5-pro'}],current:{status:'ready',kind:'chatgpt',id:'official',name:'工作账号'},activeId:'official',switch:{phase:'idle'},canSwitch:true,codexHome:'/Users/try2love/.codex'};
window.fixtureAccounts=data;
window.fixtureAccountRequest=async value=>{
 if(value.action==='models')return {...localize(data),models:['gemini-2.5-flash','gemini-2.5-pro']};
 if(value.action==='details'&&value.section==='models'){const row=data.accounts.find(r=>r.id===value.id);row.details||={};row.details.models={status:'ready',models:(row.kind==='api'?['gemini-2.5-flash','gemini-2.5-pro']:['gpt-6.1-sol','gpt-6-astra']).map(id=>({id,name:id}))};}
 if(value.action==='scan')data.discovery={candidates:[{id:'source-api',kind:'api',name:'本机 API 配置',baseUrl:'https://api.example.com/v1',model:'gemini-2.5-pro',source:'/Users/try2love/.codex'}],warnings:[]};
 if(value.action==='switch'){const row=data.accounts.find(r=>r.id===value.id);data.current={status:'ready',kind:row.kind,id:row.id,name:row.name};data.activeId=row.id;}
 return localize(data);
};
window.fixtureUsage=()=>({visible:true,accountKey:'example',email:'work@example.com',planType:'Plus',...usage(82,64,3),canReset:false,updatedAt:Date.now()/1000});
})();
