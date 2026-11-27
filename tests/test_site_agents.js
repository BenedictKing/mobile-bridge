'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');

const agents=['Codex','Kimi','MiniMax','Claude','DeepSeek','ZCode','OpenCode','WorkBuddy'];
test('README and site expose the complete built-in and custom ACP agent surface',()=>{
  for(const file of ['README.md','README_EN.md','site/index.html']){
    const source=readFileSync(require.resolve('../'+file),'utf8');
    for(const agent of agents)assert.ok(source.includes(agent),file+' missing '+agent);
    assert.ok(source.includes('acp-agent'),file+' missing custom acp-agent');
    assert.ok(source.includes('CodeBuddy'),file+' missing CodeBuddy naming');
  }
  const site=readFileSync(require.resolve('../site/index.html'),'utf8');
  assert.ok(site.includes('id="agents"'));assert.ok(site.includes('data-agent-doc'));
  assert.ok(site.includes('ACP_AGENT_BIN'));assert.ok(site.includes('尚未验证'));
  assert.ok(site.includes('Agent / ACP 运行时'));
  assert.ok(!site.includes('Codex、Kimi、Claude 等多 Agent'));
});
test('site language switch keeps the detailed agent guide bilingual',()=>{
  const script=readFileSync(require.resolve('../site/site.js'),'utf8');
  assert.ok(script.includes("data-agent-doc"));assert.ok(script.includes('README_EN.md#multiple-agents-source-runs'));
  assert.ok(script.includes('WorkBuddy and custom ACP agents'));
});
