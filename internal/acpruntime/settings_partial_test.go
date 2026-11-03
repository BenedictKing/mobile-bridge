package acpruntime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// settingsAgent 是一个行为由 marker 文件驱动的假 Agent：drop-thinking 让
// 模型切换的权威响应丢弃 thought_level 选项（mcode 行为），effort-fails 让
// 思考强度切换回 JSON-RPC 错误，no-evidence 让响应不带 configOptions。
// 每个 session/set_config_option 请求都记录到 base/log.txt 供断言顺序。
const settingsAgentScript = `import json,sys,os
base=sys.argv[1]
log=open(base+"/log.txt","a",encoding="utf-8")
def marker(name): return os.path.exists(base+"/"+name)
CONFIG=[
 {"id":"model","category":"model","currentValue":"kimi-k2","options":[{"value":"kimi-k2","name":"K2"},{"value":"kimi-k3","name":"K3"}]},
 {"id":"thought_level","category":"thought_level","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},
]
# STATE 是会话累计状态：权威响应必须反映真实 Agent 的累计事实，而不是
# 每次从初始目录重建（那会把先前已切换的模型「权威地」退回旧值）。
STATE=json.loads(json.dumps(CONFIG))
def reply(ident,result): print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result},ensure_ascii=False),flush=True)
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize":
  reply(ident,{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[],"configOptions":CONFIG})
 elif method=="session/new":
  reply(ident,{"sessionId":"sess-settings"})
 elif method=="session/set_config_option":
  log.write(json.dumps(m,ensure_ascii=False)+"\n"); log.flush()
  p=m.get("params") or {}
  if p.get("configId")=="thought_level" and marker("effort-fails"):
   print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32000,"message":"额度不足"}},ensure_ascii=False),flush=True)
   continue
  for o in STATE:
   if o["id"]==p.get("configId"): o["currentValue"]=p.get("value")
  cfg=json.loads(json.dumps(STATE))
  if p.get("configId")=="model":
   if marker("no-evidence"):
    reply(ident,{})
    continue
   if marker("drop-thinking"):
    cfg=[o for o in cfg if o["id"]!="thought_level"]
    STATE=[o for o in STATE if o["id"]!="thought_level"]
  reply(ident,{"configOptions":cfg})
 else:
  reply(ident,{})
`

func settingsSession(t *testing.T) (manager *Manager, session *Session, base string, logText func() string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	base = t.TempDir()
	script := filepath.Join(base, "agent.py")
	if err := os.WriteFile(script, []byte(settingsAgentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	manager = NewCommand([]string{python, "-u", script, base})
	manager.EffortMap = map[string]string{"low": "low", "high": "high"}
	t.Cleanup(func() { manager.Close() })
	session, err = manager.Create("request-id", "/work", "设置会话", 3*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return manager, session, base, func() string {
		data, _ := os.ReadFile(filepath.Join(base, "log.txt"))
		return string(data)
	}
}

func markSettingsAgent(t *testing.T, base, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(base, name), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshotField(session *Session, key string) string {
	text, _ := session.Snapshot().MustGet(key).(string)
	return text
}

// TestUpdateSettingsModelDropsThinkingOption 复现过期快照缺陷：模型切换的
// 权威响应丢弃 thought_level 后，不得再对已消失的选项发 RPC，且模型已生效
// 必须以部分成功申报。
func TestUpdateSettingsModelDropsThinkingOption(t *testing.T) {
	manager, session, base, logText := settingsSession(t)
	markSettingsAgent(t, base, "drop-thinking")
	confirmed, err := manager.UpdateSettings(session, "kimi-k3", "high", nil)
	var partial *SettingsPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("应为部分成功错误，got=%v", err)
	}
	if len(partial.Applied) != 1 || partial.Applied[0] != "模型" {
		t.Fatalf("Applied=%v，应为 [模型]", partial.Applied)
	}
	if !strings.Contains(err.Error(), "不提供思考强度选项") {
		t.Fatalf("错误应说明思考强度不可用：%v", err)
	}
	if confirmed {
		t.Fatal("部分成功不得报告 confirmed")
	}
	// 旧实现会拿入口快照继续给 thought_level 发 RPC。
	log := logText()
	if strings.Count(log, "set_config_option") != 1 || !strings.Contains(log, `"configId": "model"`) {
		t.Fatalf("应只发一次模型切换：%s", log)
	}
	if model := snapshotField(session, "model"); model != "kimi-k3" {
		t.Fatalf("模型应已切换：%s", model)
	}
}

// TestUpdateSettingsEffortFailureReportsPartial 思考强度 RPC 失败时申报
// 「模型已生效」的部分成功，而不是笼统失败。
func TestUpdateSettingsEffortFailureReportsPartial(t *testing.T) {
	manager, session, base, logText := settingsSession(t)
	markSettingsAgent(t, base, "effort-fails")
	_, err := manager.UpdateSettings(session, "kimi-k3", "high", nil)
	var partial *SettingsPartialError
	if !errors.As(err, &partial) || len(partial.Applied) != 1 || partial.Applied[0] != "模型" {
		t.Fatalf("应为模型已生效的部分成功：%v", err)
	}
	if !strings.Contains(err.Error(), "已生效：模型") || !strings.Contains(err.Error(), "额度不足") {
		t.Fatalf("错误应申报已生效项与失败原因：%v", err)
	}
	if got := logText(); strings.Count(got, "set_config_option") != 2 {
		t.Fatalf("应先模型后思考强度两次 RPC：%s", got)
	}
	if model := snapshotField(session, "model"); model != "kimi-k3" {
		t.Fatalf("模型应已切换：%s", model)
	}
}

// TestUpdateSettingsValidatesBeforeAnyRPC 前置校验：任何值被拒都不发 RPC、
// 不落部分成功。
func TestUpdateSettingsValidatesBeforeAnyRPC(t *testing.T) {
	manager, session, _, logText := settingsSession(t)
	for _, item := range []struct{ model, effort, needle string }{
		{"kimi-k3", "ultra", "此 Agent 不支持所选思考强度"},
		{"unknown-model", "high", "此 Agent 不支持所选模型"},
	} {
		_, err := manager.UpdateSettings(session, item.model, item.effort, nil)
		if err == nil || !strings.Contains(err.Error(), item.needle) {
			t.Fatalf("%s+%s: %v", item.model, item.effort, err)
		}
		var partial *SettingsPartialError
		if errors.As(err, &partial) {
			t.Fatalf("校验失败不得是部分成功：%v", err)
		}
	}
	if log := logText(); log != "" {
		t.Fatalf("校验失败不得发出任何 RPC：%s", log)
	}
}

// TestUpdateSettingsEvidenceFlag confirmed 只在 agent 给出权威 configOptions
// 证据时为真；无证据的乐观落账、以及全 no-op 调用各有其语义。
func TestUpdateSettingsEvidenceFlag(t *testing.T) {
	// 无证据：乐观落账，confirmed=false。
	manager, session, base, _ := settingsSession(t)
	markSettingsAgent(t, base, "no-evidence")
	confirmed, err := manager.UpdateSettings(session, "kimi-k3", "", nil)
	if err != nil || confirmed {
		t.Fatalf("无证据应为 (false,nil)：confirmed=%v err=%v", confirmed, err)
	}
	if model := snapshotField(session, "model"); model != "kimi-k3" {
		t.Fatalf("乐观落账应更新视图：%s", model)
	}

	// 权威证据：confirmed=true，模型与思考强度都生效。
	manager2, session2, _, log2 := settingsSession(t)
	confirmed, err = manager2.UpdateSettings(session2, "kimi-k3", "high", nil)
	if err != nil || !confirmed {
		t.Fatalf("权威响应应为 (true,nil)：confirmed=%v err=%v", confirmed, err)
	}
	if strings.Count(log2(), "set_config_option") != 2 {
		t.Fatalf("模型与思考强度各一次：%s", log2())
	}
	if model := snapshotField(session2, "model"); model != "kimi-k3" {
		t.Fatalf("模型=%s", model)
	}
	if effort := snapshotField(session2, "effort"); effort != "high" {
		t.Fatalf("思考强度=%s", effort)
	}

	// 全 no-op：不发现 RPC，confirmed=true（广告位即证据）。
	manager3, session3, _, log3 := settingsSession(t)
	confirmed, err = manager3.UpdateSettings(session3, "kimi-k2", "low", nil)
	if err != nil || !confirmed {
		t.Fatalf("no-op 应为 (true,nil)：confirmed=%v err=%v", confirmed, err)
	}
	if log := log3(); log != "" {
		t.Fatalf("no-op 不得发出 RPC：%s", log)
	}
}
