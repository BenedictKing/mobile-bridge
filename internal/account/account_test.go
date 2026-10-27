package account

import (
	"encoding/json"
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
 elif method=='account/rateLimits/read': result={'rateLimitsByLimitId':{'codex':{'limitName':'Codex','primary':{'usedPercent':25,'windowDurationMins':60,'resetsAt':10}}},'rateLimitResetCredits':{'availableCount':2,'credits':[{'id':'credit-1','status':'available'}]}}
 elif method=='account/rateLimitResetCredit/consume': result={'outcome':'reset'}
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
	credits := object(result, "resetCredits")
	if credits == nil || credits.MustGet("availableCount") != json.Number("2") {
		t.Fatalf("reset credits missing: %s", jsoncompat.EncodeCompact(result))
	}
	if rows, ok := credits.MustGet("credits").(*jsoncompat.Array); !ok || len(rows.Items) != 1 {
		t.Fatalf("reset credit details missing: %s", jsoncompat.EncodeCompact(credits))
	}
	accountKey, _ := result.MustGet("accountKey").(string)
	reset, err := manager.Consume(jsonObject(map[string]any{"confirmed": true, "requestId": "request-1", "accountKey": accountKey}))
	if err != nil || reset.MustGet("outcome") != "reset" {
		t.Fatalf("reset=%v err=%v", reset, err)
	}
}

func jsonObject(values map[string]any) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for key, value := range values {
		result.Set(key, value)
	}
	return result
}
