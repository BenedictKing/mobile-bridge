package jev

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssessTimeoutClampsToLadder(t *testing.T) {
	last := assessTimeouts[len(assessTimeouts)-1]
	if last != 30*time.Second {
		t.Fatalf("ladder ceiling = %s, want 30s", last)
	}
	if got := assessTimeout(-1); got != assessTimeouts[0] {
		t.Fatalf("negative attempt = %s", got)
	}
	if got := assessTimeout(len(assessTimeouts) + 3); got != last {
		t.Fatalf("overflow attempt = %s, want ceiling", got)
	}
	for i := 1; i < len(assessTimeouts); i++ {
		if assessTimeouts[i] <= assessTimeouts[i-1] {
			t.Fatalf("ladder must grow: %v", assessTimeouts)
		}
	}
}

// A slow first reply times out on the short rung and is marked Retrying; the
// next attempt gets the longer deadline and auto-approves.
func TestEvaluateSessionAttemptWidensDeadline(t *testing.T) {
	previous := assessTimeouts
	assessTimeouts = []time.Duration{50 * time.Millisecond, 2 * time.Second}
	defer func() { assessTimeouts = previous }()
	var calls int32
	echo := echoOutcomeServer(t, new(int32))
	defer echo.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(200 * time.Millisecond)
		echo.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	settings := cacheSettings(t, server.URL)
	params := permissionCard("", "ls -la", "allow_once", "opt-1")

	first := settings.EvaluateSessionAttempt("claude", "s1", "r1", "session/request_permission", params, 0)
	if first.Reason != "assess_failed" || !first.Retrying || first.Attempt != 0 {
		t.Fatalf("first attempt = %+v, want retrying assess_failed", first)
	}
	annotation := Annotate("session/request_permission", params, first)
	if annotation == nil || annotation.Assessment.MustGet("retrying") != true {
		t.Fatal("failed attempt with rungs left must annotate retrying=true")
	}
	second := settings.EvaluateSessionAttempt("claude", "s1", "r1", "session/request_permission", params, 1)
	if !second.Approved || second.Retrying {
		t.Fatalf("second attempt = %+v, want approved", second)
	}
}

func TestEvaluateSessionAttemptStopsAtLastRung(t *testing.T) {
	previous := assessTimeouts
	assessTimeouts = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}
	defer func() { assessTimeouts = previous }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	settings := cacheSettings(t, server.URL)
	params := permissionCard("", "ls", "allow_once", "opt-1")
	last := settings.EvaluateSessionAttempt("claude", "s1", "r1", "session/request_permission", params, len(assessTimeouts)-1)
	if last.Reason != "assess_failed" || last.Retrying {
		t.Fatalf("last rung = %+v, want final assess_failed", last)
	}
	if Annotate("session/request_permission", params, last).Assessment.MustGet("retrying") != false {
		t.Fatal("final failure must annotate retrying=false")
	}
}
