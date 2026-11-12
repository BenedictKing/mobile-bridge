package swarmeval

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffFrozenContentNewDeletedBinaryAndModes(t *testing.T) {
	r, source := fixture(t)
	ctx := context.Background()
	writeTest(t, source, "quotes \"and spaces\".txt", "old\n")
	writeTest(t, source, "二进制.bin", "\x00old\x01")
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "--quiet", "-m", "extra baseline")
	writeTest(t, source, "a.txt", "changed\n")
	writeTest(t, source, "new file.txt", "untracked\n")
	writeTest(t, source, "quotes \"and spaces\".txt", "new\n")
	writeTest(t, source, "二进制.bin", "\x00new\x02")
	if err := os.Remove(filepath.Join(source, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "a.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	a := captureTest(t, r, "diff-candidate", source)
	before := gitTest(t, source, "status", "--porcelain=v1")
	patch, truncated, err := r.Diff(ctx, a.ID)
	if err != nil || truncated {
		t.Fatalf("diff: %v %v", truncated, err)
	}
	for _, want := range []string{"diff --git a/a.txt b/a.txt", "-base-a", "+changed", "deleted file mode 100644", `diff --git "a/new file.txt" "b/new file.txt"`, "+untracked", "GIT binary patch", "old mode 100644", "new mode 100755"} {
		if !strings.Contains(patch, want) {
			t.Fatalf("missing %q: %s", want, patch)
		}
	}
	if strings.Contains(patch, source) || strings.Contains(patch, r.Root) || strings.Contains(patch, "a/base/") || strings.Contains(patch, "b/tree/") {
		t.Fatalf("private path leaked: %s", patch)
	}
	if after := gitTest(t, source, "status", "--porcelain=v1"); before != after {
		t.Fatal("diff changed source")
	}
	// 规范化结果仍是可实际应用的完整 binary patch，含特殊文件名和执行位。
	apply := t.TempDir()
	gitTest(t, apply, "clone", "--quiet", source, ".")
	cmd := exec.Command("git", "-C", apply, "apply", "--binary", "-")
	cmd.Stdin = strings.NewReader(patch)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply normalized patch: %s %v\n%s", output, err, patch)
	}
	for _, name := range []string{"a.txt", "new file.txt", "quotes \"and spaces\".txt", "二进制.bin"} {
		got, err := os.ReadFile(filepath.Join(apply, name))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(filepath.Join(a.Directory, name))
		if string(got) != string(want) {
			t.Fatalf("applied content mismatch: %s", name)
		}
	}
}

func TestDiffRejectsTamperingAndBoundsOutput(t *testing.T) {
	r, source := fixture(t)
	writeTest(t, source, "large.txt", strings.Repeat("a long added line\n", 40000))
	a := captureTest(t, r, "large-patch", source)
	patch, truncated, err := r.Diff(context.Background(), a.ID)
	if err != nil || !truncated || len(patch) > MaxDiffBytes {
		t.Fatalf("bound: %d %v %v", len(patch), truncated, err)
	}
	if _, _, err := r.Diff(context.Background(), "../escape"); err == nil {
		t.Fatal("untrusted ID accepted")
	}
	writeTest(t, a.Directory, "a.txt", "tampered")
	if _, _, err := r.Diff(context.Background(), a.ID); !errors.Is(err, ErrChanged) {
		t.Fatalf("tamper accepted: %v", err)
	}
}

func TestDiffIdenticalCandidateReturnsEmpty(t *testing.T) {
	r, source := fixture(t)
	a := captureTest(t, r, "unchanged", source)
	patch, truncated, err := r.Diff(context.Background(), a.ID)
	if err != nil || truncated || patch != "" {
		t.Fatalf("unchanged: %q %v %v", patch, truncated, err)
	}
}
