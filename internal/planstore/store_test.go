package planstore

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gateway", "acp-plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	return store
}

func record(t *testing.T, store *Store, session string, version int, content string) {
	t.Helper()
	if err := store.Record("kimi", session, "plan-1", version, Hash(content), content); err != nil {
		t.Fatal(err)
	}
}

func TestStorePersistsRevisionChainWithoutReplayGrowth(t *testing.T) {
	store := testStore(t)
	record(t, store, "session-1", 1, "first")
	record(t, store, "session-1", 1, "first")
	record(t, store, "session-1", 2, "second")
	record(t, store, "session-2", 1, "other session")

	reopened, err := Open(store.path)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := reopened.Get("kimi", "session-1", "plan-1")
	if !ok || len(plan.Revisions) != 2 || plan.Revisions[0].Content != "first" || plan.Revisions[1].Content != "second" {
		t.Fatalf("unexpected persisted plan: %+v", plan)
	}
	if plan.Revisions[0].CreatedAt.Equal(plan.Revisions[1].CreatedAt) {
		t.Fatal("identical replay must not add a revision")
	}
	if _, exists := reopened.Get("kimi", "session-2", "plan-1"); !exists {
		t.Fatal("same plan id in another session must remain independent")
	}
	if _, exists := reopened.Get("claude", "session-1", "plan-1"); exists {
		t.Fatal("agent namespace leaked")
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("plan store permissions = %o", info.Mode().Perm())
	}
}

func TestStoreAllowsMissingVersionsAndReturnsCopies(t *testing.T) {
	store := testStore(t)
	record(t, store, "session-1", 2, "persisted after a gap")
	plan, ok := store.Get("kimi", "session-1", "plan-1")
	if !ok || len(plan.Revisions) != 1 || plan.Revisions[0].Version != 2 {
		t.Fatalf("gap was not retained: %+v", plan)
	}
	plan.Revisions[0].Content = "mutated"
	again, _ := store.Get("kimi", "session-1", "plan-1")
	if again.Revisions[0].Content != "persisted after a gap" {
		t.Fatal("caller mutated store history")
	}
	if err := store.Record("kimi", "session-1", "plan-1", 2, Hash("different"), "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same version with different content = %v", err)
	}
}

func TestStoreTrimsOldestRevisionsAndBytes(t *testing.T) {
	t.Run("revision count", func(t *testing.T) {
		store := testStore(t)
		for version := 1; version <= MaxRevisions+1; version++ {
			record(t, store, "session-1", version, "revision")
		}
		plan, _ := store.Get("kimi", "session-1", "plan-1")
		if len(plan.Revisions) != MaxRevisions || plan.Revisions[0].Version != 2 || plan.Revisions[len(plan.Revisions)-1].Version != MaxRevisions+1 {
			t.Fatalf("revision trim boundary: count=%d first=%d", len(plan.Revisions), plan.Revisions[0].Version)
		}
	})
	t.Run("content bytes", func(t *testing.T) {
		store := testStore(t)
		for version := 1; version <= 50; version++ {
			record(t, store, "session-1", version, strings.Repeat("x", 200000))
		}
		plan, _ := store.Get("kimi", "session-1", "plan-1")
		if planBytes(plan) > MaxPlanBytes || plan.Revisions[len(plan.Revisions)-1].Version != 50 {
			t.Fatalf("byte trim boundary: bytes=%d versions=%d", planBytes(plan), len(plan.Revisions))
		}
	})
}

func TestStoreRejectsInvalidContentAndKeepsCorruptFile(t *testing.T) {
	store := testStore(t)
	oversized := strings.Repeat("x", MaxContentBytes+1)
	if err := store.Record("kimi", "session-1", "plan-1", 1, Hash(oversized), oversized); err == nil {
		t.Fatal("oversized revision was accepted")
	}
	path := store.path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{not-json\n")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt store was accepted")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(corrupt) {
		t.Fatalf("corrupt store was changed: %q %v", raw, err)
	}
}

func TestStoreRetriesFailedSaveOnIdenticalReplay(t *testing.T) {
	store := testStore(t)
	blockedPath := store.path
	store.path = filepath.Dir(blockedPath)
	if err := os.MkdirAll(store.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Record("kimi", "session-1", "plan-1", 1, Hash("first"), "first"); err == nil {
		t.Fatal("save to a directory unexpectedly succeeded")
	}
	store.path = blockedPath
	if err := store.Record("kimi", "session-1", "plan-1", 1, Hash("first"), "first"); err != nil {
		t.Fatalf("identical replay did not retry dirty save: %v", err)
	}
	reopened, err := Open(blockedPath)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := reopened.Get("kimi", "session-1", "plan-1")
	if !ok || len(plan.Revisions) != 1 || plan.Revisions[0].Content != "first" {
		t.Fatalf("recovered plan = %+v, %v", plan, ok)
	}
}

func TestStoreRecordsFeedbackPerVersionOnce(t *testing.T) {
	store := testStore(t)
	record(t, store, "session-1", 1, "first")
	record(t, store, "session-1", 2, "second")
	feedback := Feedback{RequestID: "req-1", Text: "整体精简", Annotations: []Annotation{
		{ID: "A1", Type: "comment", Quote: "first", Body: "改写"},
		{ID: "A2", Type: "delete", Quote: "first"},
	}}
	if err := store.RecordFeedback("kimi", "session-1", "plan-1", 1, Hash("first"), feedback); err != nil {
		t.Fatal(err)
	}
	// 命令重放携带同一请求 ID，只记一次。
	if err := store.RecordFeedback("kimi", "session-1", "plan-1", 1, Hash("first"), feedback); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(store.RecordFeedback("kimi", "session-1", "plan-1", 2, Hash("first"), feedback), ErrConflict) {
		t.Fatal("hash mismatch must conflict")
	}
	if store.RecordFeedback("kimi", "session-1", "plan-1", 3, Hash("third"), feedback) == nil {
		t.Fatal("unknown version must not be invented")
	}
	if store.RecordFeedback("kimi", "session-2", "plan-1", 1, Hash("first"), feedback) == nil {
		t.Fatal("feedback must not cross sessions")
	}

	reopened, err := Open(store.path)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := reopened.Get("kimi", "session-1", "plan-1")
	got := plan.Revisions[0].Feedback
	if len(got) != 1 || got[0].RequestID != "req-1" || got[0].CreatedAt.IsZero() || len(got[0].Annotations) != 2 || len(plan.Revisions[1].Feedback) != 0 {
		t.Fatalf("persisted feedback=%+v", plan.Revisions)
	}
	// Get 返回深拷贝：修改调用方副本不影响库内数据。
	got[0].Annotations[0].Body = "mutated"
	again, _ := reopened.Get("kimi", "session-1", "plan-1")
	if again.Revisions[0].Feedback[0].Annotations[0].Body != "改写" {
		t.Fatal("Get leaked internal feedback slices")
	}
}

func TestStoreCapsFeedbackPerRevisionAndPlanSize(t *testing.T) {
	store := testStore(t)
	record(t, store, "session-1", 1, "first")
	for index := 0; index < MaxFeedbackPerRev+3; index++ {
		feedback := Feedback{RequestID: "req-" + strings.Repeat("x", index+1), Text: "意见"}
		if err := store.RecordFeedback("kimi", "session-1", "plan-1", 1, Hash("first"), feedback); err != nil {
			t.Fatal(err)
		}
	}
	plan, _ := store.Get("kimi", "session-1", "plan-1")
	if got := plan.Revisions[0].Feedback; len(got) != MaxFeedbackPerRev || got[0].RequestID != "req-xxxx" {
		t.Fatalf("feedback cap kept %d, first=%s", len(got), got[0].RequestID)
	}

	big := strings.Repeat("a", MaxContentBytes)
	if err := store.Record("kimi", "session-2", "plan-1", 1, Hash(big), big); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("意", MaxFeedbackRunes)
	annotations := []Annotation{}
	for index := 1; index <= MaxAnnotations; index++ {
		annotations = append(annotations, Annotation{ID: "A" + strconv.Itoa(index), Type: "comment", Quote: "a", Body: body})
	}
	// 单个版本无法再裁剪：2 MiB 正文叠加多次约 3 MB 的批注终会越过 8 MiB，
	// 越界那次必须拒绝，避免落盘后整个历史库无法加载。
	rejected := false
	for index := 0; index < 4 && !rejected; index++ {
		feedback := Feedback{RequestID: "req-big-" + strconv.Itoa(index), Annotations: annotations}
		rejected = store.RecordFeedback("kimi", "session-2", "plan-1", 1, Hash(big), feedback) != nil
	}
	single, _ := store.Get("kimi", "session-2", "plan-1")
	if !rejected || planBytes(single) > MaxPlanBytes {
		t.Fatalf("oversized feedback rejected=%v bytes=%d", rejected, planBytes(single))
	}
	if _, err := Open(store.path); err != nil {
		t.Fatalf("store must stay loadable: %v", err)
	}
}

func TestValidateFeedbackRejectsMalformedAnnotations(t *testing.T) {
	valid := Annotation{ID: "A1", Type: "comment", Quote: "q", Body: "b"}
	cases := map[string]Feedback{
		"empty":         {RequestID: "r"},
		"blank text":    {RequestID: "r", Text: "  "},
		"no request":    {Text: "x"},
		"bad id":        {RequestID: "r", Annotations: []Annotation{{ID: "B1", Type: "comment", Quote: "q", Body: "b"}}},
		"zero id":       {RequestID: "r", Annotations: []Annotation{{ID: "A0", Type: "comment", Quote: "q", Body: "b"}}},
		"duplicate id":  {RequestID: "r", Annotations: []Annotation{valid, valid}},
		"bad type":      {RequestID: "r", Annotations: []Annotation{{ID: "A1", Type: "replace", Quote: "q", Body: "b"}}},
		"empty quote":   {RequestID: "r", Annotations: []Annotation{{ID: "A1", Type: "delete", Quote: " "}}},
		"comment blank": {RequestID: "r", Annotations: []Annotation{{ID: "A1", Type: "comment", Quote: "q", Body: " "}}},
		"long quote":    {RequestID: "r", Annotations: []Annotation{{ID: "A1", Type: "delete", Quote: strings.Repeat("q", MaxQuoteRunes+1)}}},
		"long text":     {RequestID: "r", Text: strings.Repeat("t", MaxFeedbackRunes+1)},
	}
	for name, feedback := range cases {
		if ValidateFeedback(feedback) == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if err := ValidateFeedback(Feedback{RequestID: "r", Annotations: []Annotation{valid, {ID: "A2", Type: "delete", Quote: "q"}}}); err != nil {
		t.Fatalf("valid feedback rejected: %v", err)
	}
}
