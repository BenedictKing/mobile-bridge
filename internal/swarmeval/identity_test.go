package swarmeval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSameRepositoryAcceptsOnlyRootsOfSharedRepository(t *testing.T) {
	_, source := fixture(t)
	ctx := context.Background()
	if err := SameRepository(ctx, source, source); err != nil {
		t.Fatal(err)
	}
	// 只在独立临时仓库创建测试 worktree，不触碰项目或用户工作区。
	worktree := filepath.Join(t.TempDir(), "checkout")
	gitTest(t, source, "worktree", "add", "--detach", worktree, "HEAD")
	if err := SameRepository(ctx, source, worktree); err != nil {
		t.Fatalf("registered worktree: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(source, alias); err == nil {
		if err := SameRepository(ctx, alias, worktree); err != nil {
			t.Fatalf("real path: %v", err)
		}
	}
	clone := t.TempDir()
	gitTest(t, clone, "clone", "--quiet", source, ".")
	if err := SameRepository(ctx, source, clone); err == nil {
		t.Fatal("independent clone accepted")
	}
	subdir := filepath.Join(source, "nested")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := SameRepository(ctx, subdir, source); err == nil {
		t.Fatal("subdirectory accepted as root")
	}
	if err := SameRepository(ctx, t.TempDir(), source); err == nil {
		t.Fatal("nonrepository accepted")
	}
	if err := SameRepository(ctx, "", source); err == nil {
		t.Fatal("empty root accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := SameRepository(cancelled, source, source); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestChangedPathsBetweenPreservesExistingUserChanges(t *testing.T) {
	r, source := fixture(t)
	ctx := context.Background()
	writeTest(t, source, "a.txt", "preexisting-user-change\n")
	writeTest(t, source, "user.txt", "preexisting-untracked\n")
	before := captureTest(t, r, "before", source)
	writeTest(t, source, "b.txt", "worker-change\n")
	writeTest(t, source, "worker.txt", "worker-added\n")
	after := captureTest(t, r, "after", source)
	want := []string{"b.txt", "worker.txt"}
	for _, compare := range []func(context.Context, *Artifact, *Artifact) ([]string, error){ChangedPathsBetween, r.ChangedPathsBetween} {
		paths, err := compare(ctx, before, after)
		if err != nil || !reflect.DeepEqual(paths, want) {
			t.Fatalf("actual task paths: %v %v", paths, err)
		}
	}
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "--quiet", "-m", "commit worker and user changes")
	committed := captureTest(t, r, "committed", source)
	paths, err := ChangedPathsBetween(ctx, after, committed)
	if err != nil || len(paths) != 0 {
		t.Fatalf("same content commit: %v %v", paths, err)
	}
	if err := os.Remove(filepath.Join(source, "worker.txt")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, source, "user.txt", "worker-rewrote-user-file\n")
	deleted := captureTest(t, r, "deleted", source)
	paths, err = ChangedPathsBetween(ctx, committed, deleted)
	if err != nil || !reflect.DeepEqual(paths, []string{"user.txt", "worker.txt"}) {
		t.Fatalf("rewrite and delete: %v %v", paths, err)
	}
	data, err := os.ReadFile(filepath.Join(source, "a.txt"))
	if err != nil || string(data) != "preexisting-user-change\n" {
		t.Fatal("comparison changed source")
	}
}

func TestChangedPathsBetweenRejectsTamperingAndTracksExecutableMode(t *testing.T) {
	r, source := fixture(t)
	ctx := context.Background()
	before := captureTest(t, r, "before", source)
	if err := os.Chmod(filepath.Join(source, "a.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	after := captureTest(t, r, "after", source)
	paths, err := ChangedPathsBetween(ctx, before, after)
	if err != nil || !reflect.DeepEqual(paths, []string{"a.txt"}) {
		t.Fatalf("executable mode: %v %v", paths, err)
	}
	writeTest(t, after.Directory, "a.txt", "tampered\n")
	if _, err := ChangedPathsBetween(ctx, before, after); !errors.Is(err, ErrChanged) {
		t.Fatalf("tamper accepted: %v", err)
	}
	if _, err := ChangedPathsBetween(ctx, nil, before); err == nil {
		t.Fatal("nil accepted")
	}
	forged := *before
	forged.ContentHash = "not-the-actual-hash"
	if _, err := r.ChangedPathsBetween(ctx, &forged, before); !errors.Is(err, ErrConflict) {
		t.Fatalf("forged identity accepted: %v", err)
	}
}
