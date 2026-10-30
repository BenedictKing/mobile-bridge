package team

import (
	"errors"
	"testing"
)

type fakeRecreatingResolver struct {
	*fakeResolver
	newID string
	err   error
	calls int
}

func (r *fakeRecreatingResolver) Recreate(agentKind, threadID, cwd string) (string, error) {
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	r.setSession(r.newID, newFakeSession(r.newID, cwd))
	r.setCwd(r.newID, cwd)
	return r.newID, nil
}

func TestDispatchRecreatesMissingSessionAndRebindsMember(t *testing.T) {
	h := newActionsExecHarness(t)
	recreator := &fakeRecreatingResolver{fakeResolver: h.resolver, newID: "s-new"}
	h.service.resolver = recreator
	h.resolver.setUnopenable("s-dev", errors.New("Session not found"))
	run := &Run{ID: "run-ex", TeamID: h.team.ID, Status: RunDispatching, Tasks: []Task{{
		ID: "task", MemberID: "dev", Prompt: "继续执行", Status: TaskPlanned, WriteLock: TaskWriteLockNone,
	}}}
	state := h.saveExecRun(t, run)
	h.service.executeDispatch(state, h.team, h.team.Policy, h.team.ID, run.ID, "task", "/tmp/dev")

	if recreator.calls != 1 {
		t.Fatalf("丢失会话应只重建一次，实际 %d", recreator.calls)
	}
	if member := memberOf(h.store.Team(h.team.ID), "dev"); member.SessionID != "s-new" {
		t.Fatalf("成员绑定未回写: %+v", member)
	}
	entry, ok := h.queue.firstEntry("s-new")
	if !ok || entry.text == "" {
		t.Fatalf("原任务未派发到新会话: %+v", entry)
	}
	if len(h.queue.entriesFor("s-dev")) != 0 {
		t.Fatal("恢复后不得继续向旧会话派发")
	}
}

func TestSessionRecoveryFailureFallsBackToLeadTriage(t *testing.T) {
	h := newActionsExecHarness(t)
	recreator := &fakeRecreatingResolver{fakeResolver: h.resolver, newID: "s-new", err: errors.New("create failed")}
	h.service.resolver = recreator
	h.resolver.setUnopenable("s-dev", errors.New("Session not found"))
	run := execRunFixture()
	run.Tasks = append(run.Tasks, Task{
		ID: "dev-task", MemberID: "dev", Prompt: "继续执行", Status: TaskPlanned, WriteLock: TaskWriteLockNone,
	})
	state := h.saveExecRun(t, run)
	h.service.executeDispatch(state, h.team, h.team.Policy, h.team.ID, run.ID, "dev-task", "/tmp/dev")
	if task := findTaskIn(h.store.Run(run.ID), "dev-task"); task.Status != TaskFailed {
		t.Fatalf("恢复失败应落原任务失败: %+v", task)
	}
	h.service.passFold(state)
	if triage := findTaskIn(h.store.Run(run.ID), "lead-triage-1"); triage == nil {
		t.Fatal("恢复失败后应交给 Lead 分诊")
	}
	if recreator.calls != 1 || len(h.queue.entriesFor("s-dev")) != 0 {
		t.Fatal("恢复失败不得重复重建或向旧会话派发")
	}
}

func TestSessionNotFoundMatchIsSpecific(t *testing.T) {
	if !sessionNotFound(errors.New("session/load: Session not found")) {
		t.Fatal("明确的会话缺失应触发恢复")
	}
	if sessionNotFound(errors.New("permission denied")) || sessionNotFound(nil) {
		t.Fatal("其他打开失败不得触发会话重建")
	}
}
