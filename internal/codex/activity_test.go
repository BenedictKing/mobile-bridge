package codex

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestActivityReadsCanonicalTurnHistory(t *testing.T) {
	state, err := jsoncompat.Unmarshal([]byte(`{"id":"thread-1","turns":[],"turnHistory":{"kind":"canonical","history":{"entitiesByKey":{"tail:0:turn-1":{"turnId":"turn-1","status":"inProgress","items":[{"id":"answer-1","type":"agentMessage","text":"working"}]}},"islands":[{"entries":[{"value":"tail:0:turn-1"}]}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	bridge := &Bridge{Host: "local", sessions: map[string]*Session{
		"thread-1": {State: state, Connected: true},
	}}
	rows := bridge.Activity([]string{"thread-1"})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	row := rows[0]
	if got := row.MustGet("status"); got != "active" {
		t.Fatalf("status=%v, want active", got)
	}
	if got := row.MustGet("turnStatus"); got != "inProgress" {
		t.Fatalf("turnStatus=%v, want inProgress", got)
	}
	if got := row.MustGet("turnId"); got != "turn-1" {
		t.Fatalf("turnId=%v, want turn-1", got)
	}
}
