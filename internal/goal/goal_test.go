package goal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoalRPCMethodsAreWhitelisted(t *testing.T) {
	script := `#!/usr/bin/env python3
import json,sys
for line in sys.stdin:
 m=json.loads(line); ident=m.get('id'); method=m.get('method')
 if method=='initialize': result={}
 elif method=='thread/goal/get': result={'goal':{'objective':'ship','status':'active','createdAt':1}}
 elif method=='thread/goal/set': result={'goal':{'objective':m.get('params',{}).get('objective','ship'),'status':m.get('params',{}).get('status','active'),'createdAt':1}}
 elif method=='thread/goal/clear': result={}
 else: result={}
 print(json.dumps({'id':ident,'result':result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "runtime.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := New(t.TempDir(), path)
	current, err := manager.Get("thread-1")
	if err != nil || current.MustGet("objective") != "ship" {
		t.Fatalf("get=%v err=%v", current, err)
	}
	paused, err := manager.Set("thread-1", "ship safely", "paused")
	if err != nil || paused.MustGet("status") != "paused" {
		t.Fatalf("set=%v err=%v", paused, err)
	}
	if err := manager.Clear("thread-1"); err != nil {
		t.Fatal(err)
	}
}
