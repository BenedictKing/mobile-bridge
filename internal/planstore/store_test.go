package planstore

import (
	"errors"
	"os"
	"path/filepath"
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
