package team

import (
	"reflect"
	"testing"
)

func TestNormalizeTouchedPaths(t *testing.T) {
	got := NormalizeTouchedPaths([]string{"src/b.go", "src/a.go", "src/a.go", "../outside", "/absolute", "C:/drive", "src/../src/a.go"})
	want := []string{"src/a.go", "src/b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("路径归一化不符: got=%v want=%v", got, want)
	}
}

func TestReviewStaleAfterWriteUsesTouchedPaths(t *testing.T) {
	review := &Task{ReviewPaths: []string{"src/a.go"}}
	unrelated := &Task{ResultDetail: &TaskResult{TouchedPaths: []string{"src/b.go"}}}
	if reviewStaleAfterWrite(review, unrelated) {
		t.Fatal("无关路径写入不应使审查过期")
	}
	related := &Task{ResultDetail: &TaskResult{TouchedPaths: []string{"src/a.go"}}}
	if !reviewStaleAfterWrite(review, related) {
		t.Fatal("触及被审路径应使审查过期")
	}
	if !reviewStaleAfterWrite(review, &Task{}) || !reviewStaleAfterWrite(&Task{}, related) {
		t.Fatal("任一路径未知时必须保守过期")
	}
}

type fakePathFingerprint struct {
	fakeFingerprint
	pathDiff string
	paths    []string
}

func (f *fakePathFingerprint) FingerprintPaths(_ string, paths []string) (string, string, error) {
	f.paths = append([]string(nil), paths...)
	return f.head, f.pathDiff, f.err
}

func TestGoalWorkspaceEvidenceUsesContentAndReviewPaths(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.ProjectCwd = "/workspace"
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	fingerprint := &fakePathFingerprint{fakeFingerprint: fakeFingerprint{head: "head-1", diff: "full-1"}, pathDiff: "path-1"}
	h.service.Fingerprints = fingerprint
	run := &Run{ID: "run", TeamID: h.team.ID, Status: RunCompleted,
		Verify: &VerifyResult{ExitCode: 0, HeadRevision: "head-1", DiffHash: "full-1"},
		Tasks: []Task{
			{ID: "impl", MemberID: "dev", Status: TaskCompleted, ResultDetail: &TaskResult{TouchedPaths: []string{"src/b.go", "src/a.go"}}},
			{ID: "review", MemberID: "rev", Review: reviewFlag(true), ReviewOf: []string{"impl"}, Status: TaskCompleted,
				ReviewPaths: []string{"src/a.go"}, HeadRevision: "head-1", DiffHash: "path-1",
				ResultDetail: &TaskResult{ReviewVerdict: ReviewVerdictPassed}},
		}}
	contract := DefaultGoalContract(RunGoalScope(run))
	contract.Verify.Requirement, contract.Review.Requirement = "required", "required"
	fingerprint.head = "head-2"
	if _, _, err := h.service.goalWorkspaceEvidenceError(run, contract); err != nil {
		t.Fatalf("HEAD 变化本身不应使相同内容失效: %v", err)
	}
	fingerprint.pathDiff = "path-2"
	if _, _, err := h.service.goalWorkspaceEvidenceError(run, contract); err == nil {
		t.Fatal("被审路径变化应使审查失效")
	}
	fingerprint.pathDiff = "path-1"
	fingerprint.diff = "full-2"
	if _, _, err := h.service.goalWorkspaceEvidenceError(run, contract); err == nil {
		t.Fatal("全工作区验证仍应检测任意内容变化")
	}
}

func TestReviewPathsPersistThroughDispatchEvent(t *testing.T) {
	run := &Run{ID: "run", Tasks: []Task{{ID: "review"}}}
	applyJournalEventToRun(run, &Event{Type: EventTaskDispatching, Payload: map[string]any{
		"taskId": "review", "reviewPaths": []string{"src/a.go", "src/b.go"},
	}})
	if len(run.Tasks[0].ReviewPaths) != 2 || run.Tasks[0].ReviewPaths[0] != "src/a.go" {
		t.Fatalf("journal 重建丢失审查路径: %+v", run.Tasks[0].ReviewPaths)
	}
	if got := reviewTouchedPaths(run, &Task{ReviewOf: []string{"review"}}); len(got) != 0 {
		t.Fatalf("审查任务本身没有实现结果路径: %v", got)
	}
}
