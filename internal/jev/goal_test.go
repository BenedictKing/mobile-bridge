package jev

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGoalAssessAndDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"answers":{"complete":{"choice":"no"},"continue":{"choice":"continue"},"stop_kind":{"choice":"finished"},"confidence":{"noul":0.8}}}`))
	}))
	defer server.Close()
	ctx := GoalContext{Objective: "整理仓库", Agent: "kimi", GoalID: "g", Revision: 1, Step: 1, TurnID: "t", StopReason: "end_turn"}
	assessment, err := testSettings(t, server.URL).GoalAssess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status, reason := DecideGoal(ctx, assessment); status != "dispatching" || reason != "jev_continue" {
		t.Fatalf("decision=%s %s", status, reason)
	}
	if status, _ := DecideGoal(GoalContext{StopReason: "cancelled"}, assessment); status != "blocked" {
		t.Fatalf("user stop resumed: %s", status)
	}
	if status, _ := DecideGoal(GoalContext{StopReason: "end_turn", PendingRequests: true}, assessment); status != "waiting_input" {
		t.Fatalf("pending resumed: %s", status)
	}
	if status, _ := DecideGoal(ctx, nil); status != "waiting_review" {
		t.Fatalf("no verdict resumed: %s", status)
	}
}

func TestGoalAssessRejectsUnsafeReplies(t *testing.T) {
	for _, response := range []string{
		`{"answers":{"complete":{"choice":"yes"},"continue":{"choice":"continue"},"stop_kind":{"choice":"finished"},"confidence":{"noul":0.9}}}`,
		`{"answers":{"complete":{"choice":"no"},"continue":{"choice":"continue"},"stop_kind":{"choice":"finished"}}}`,
		`{"answers":{"complete":{"choice":"no"},"continue":{"choice":"continue"},"stop_kind":{"choice":"finished"},"confidence":{"noul":1.1}}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, response) }))
		if result, err := testSettings(t, server.URL).GoalAssess(GoalContext{}); err == nil || result != nil {
			t.Fatalf("unsafe reply accepted: %s: %+v", response, result)
		}
		server.Close()
	}
	if status, _ := DecideGoal(GoalContext{StopReason: "end_turn"}, &GoalAssessment{Complete: false, Continue: "continue", StopKind: "finished", Confidence: 0.79}); status != "waiting_review" {
		t.Fatalf("low confidence=%s", status)
	}
	if status, _ := DecideGoal(GoalContext{StopReason: "end_turn", Error: "quota exceeded"}, &GoalAssessment{Complete: false, Continue: "continue", StopKind: "finished", Confidence: 1}); status != "blocked" {
		t.Fatalf("quota=%s", status)
	}
}

func TestGoalStateTruncatesOutput(t *testing.T) {
	state := (GoalContext{Objective: strings.Repeat("x", 5000), AssistantTail: strings.Repeat("y", 7000)}).state()
	if got := len([]rune(state.MustGet("objective").(string))); got > 4000 {
		t.Fatalf("objective=%d", got)
	}
	if got := len([]rune(state.MustGet("assistantTail").(string))); got > 6000 {
		t.Fatalf("tail=%d", got)
	}
}
