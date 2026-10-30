package httpd

// warmup_test.go 覆盖成员会话预热：按项目建成员时，懒加载 Agent（zcode /
// workbuddy）只有首轮对话才会把会话落到自己的库里，网关必须补一条预热消息，
// 否则句柄被回收或重启后 session/load 无会话可载，成员就一直显示「不可用」。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
)

// teamsWarmupAgent 记录每条提示词并立即收尾（不像 teamsHoldAgent 那样 hold）：
// 预热用例只关心「发了什么、发给谁」。
func teamsWarmupAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys
DIR = "` + dir + `"
def read(name):
 try:
  with open(os.path.join(DIR,name)) as fh:
   return [line.strip() for line in fh if line.strip()]
 except FileNotFoundError:
  return []
def append(name, value):
 with open(os.path.join(DIR,name),"a") as fh:
  fh.write(value + "\n")
created = {"n": len(read("created.txt"))}
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id"); params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new":
  created["n"]+=1
  sid="km-warm-%d" % created["n"]
  append("created.txt", params.get("cwd","") + "\t" + sid)
  result={"sessionId":sid,"modes":{"currentModeId":"default","availableModes":[{"id":"default"}]},"configOptions":[]}
 elif method == "session/list":
  result={"sessions":[{"sessionId":line.split("\t")[-1],"cwd":DIR,"title":"成员会话","updatedAt":1} for line in read("created.txt")]}
 elif method == "session/prompt":
  text=""
  for block in params.get("prompt") or []:
   if isinstance(block,dict) and block.get("text"):
    text=block["text"]; break
  append("prompts.txt", params.get("sessionId","") + "\t" + text)
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// projectTeamMember 经 HTTP 按项目创建一个单成员团队，返回成员的会话 id。
func projectTeamMember(t *testing.T, env *teamTestEnv) string {
	t.Helper()
	env.server.Creations = LoadCreations(filepath.Join(env.dataDir, "creations.json"))
	project := "acp:" + hashProject(env.dataDir)
	payload := fmt.Sprintf(`{"name":"预热团队","members":[{"id":"lead","project":%q,"agent":"kimi","role":"lead","name":"负责人"}],"leadId":"lead"}`, project)
	response := env.request(http.MethodPost, "/api/teams", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("按项目创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		Members []struct {
			SessionID string `json:"sessionId"`
		} `json:"members"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Members) != 1 || created.Members[0].SessionID == "" || created.Members[0].SessionID == env.sessionID {
		t.Fatalf("成员会话未按项目新建: %+v", created.Members)
	}
	return created.Members[0].SessionID
}

func TestProjectTeamWarmsUpLazyMember(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true, teamsWarmupAgent)
	env.manager.LazySessions = true
	memberID := projectTeamMember(t, env)

	promptsPath := filepath.Join(env.dataDir, "prompts.txt")
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(promptsPath)
		if err == nil && strings.Contains(string(raw), acpruntime.WarmupPrompt) {
			// The warm-up must go to the member's own session, never to the
			// chat the fixture created.
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if !strings.Contains(line, acpruntime.WarmupPrompt) {
					continue
				}
				if !strings.HasPrefix(line, memberID+"\t") {
					t.Fatalf("预热发给了别的会话: %q (成员 %s)", line, memberID)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("懒加载成员没有收到预热消息（prompts=%q）", string(raw))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestProjectTeamSkipsWarmupForEagerAgent(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true, teamsWarmupAgent)
	env.manager.LazySessions = false
	projectTeamMember(t, env)

	// Nothing to wait for: a normal agent materializes the session on
	// session/new, so any prompt here would be a stray model call.
	time.Sleep(300 * time.Millisecond)
	if raw, err := os.ReadFile(filepath.Join(env.dataDir, "prompts.txt")); err == nil {
		t.Fatalf("非懒加载 Agent 不应预热: %q", string(raw))
	}
}

// TestSessionDeleteHidesLazyAliases: deleting a warmed chat by its placeholder
// must also hide the backend id the agent lists afterwards — otherwise the row
// pops straight back into the chat list.
func TestSessionDeleteHidesLazyAliases(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true, teamsWarmupAgent)
	env.server.Hidden = LoadHiddenSessions(filepath.Join(env.dataDir, "hidden-sessions.json"))
	env.manager.Canonical = func(id string) string {
		if id == env.sessionID {
			return "sess_backend"
		}
		return ""
	}
	response := env.request(http.MethodPost, "/api/sessions/"+env.sessionID+"/delete?host=local", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("删除会话失败: %d %s", response.Code, response.Body.String())
	}
	if !env.server.Hidden.Hidden(env.sessionID) || !env.server.Hidden.Hidden("sess_backend") {
		t.Fatalf("别名未一起隐藏: placeholder=%v backend=%v",
			env.server.Hidden.Hidden(env.sessionID), env.server.Hidden.Hidden("sess_backend"))
	}
}
