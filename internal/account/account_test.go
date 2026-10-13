package account

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestReadUsesRestrictedAccountRPC(t *testing.T) {
	script := `#!/usr/bin/env python3
import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get('method'); ident=m.get('id')
 if method=='initialize': result={}
 elif method=='config/read': result={'config':{'model_provider':'openai','desktop':{'agent-usage-reset-enabled':True}}}
 elif method=='account/read': result={'account':{'type':'chatgpt','email':'user@example.com','planType':'plus'},'workspaceRouting':'team'}
 elif method=='account/rateLimits/read': result={'rateLimitsByLimitId':{'codex':{'limitName':'Codex','primary':{'usedPercent':25,'windowDurationMins':60,'resetsAt':10}}}}
 else: result={}
 print(json.dumps({'id':ident,'result':result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "runtime.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := New(t.TempDir(), path)
	result := manager.Read(true)
	if result.MustGet("visible") != true || result.MustGet("loginType") != "chatgpt" {
		t.Fatalf("unexpected account: %s", jsoncompat.EncodeCompact(result))
	}
	if result.MustGet("email") != "user@example.com" {
		t.Fatalf("email missing: %s", jsoncompat.EncodeCompact(result))
	}
}
