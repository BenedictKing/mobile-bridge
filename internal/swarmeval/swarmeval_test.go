package swarmeval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(systemEnvironment(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func writeTest(t *testing.T, dir, name, value string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) (*Runner, string) {
	t.Helper()
	source := t.TempDir()
	gitTest(t, source, "init", "--quiet")
	writeTest(t, source, "a.txt", "base-a\n")
	writeTest(t, source, "b.txt", "base-b\n")
	writeTest(t, source, ".gitignore", "ignored/\n")
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "--quiet", "-m", "base")
	return &Runner{Root: t.TempDir()}, source
}
func captureTest(t *testing.T, r *Runner, id, source string) *Artifact {
	t.Helper()
	a, err := r.Capture(context.Background(), id, source, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCaptureFreshContentAndOriginalIndex(t *testing.T) {
	r, source := fixture(t)
	indexBefore, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, source, "a.txt", "changed-a\n")
	writeTest(t, source, "new.txt", "new-one\n")
	writeTest(t, source, "ignored/secret", "not copied")
	a := captureTest(t, r, "first", source)
	if strings.Join(a.ChangedPaths, ",") != "a.txt,new.txt" {
		t.Fatalf("paths: %v", a.ChangedPaths)
	}
	if _, err := os.Stat(filepath.Join(a.Directory, "ignored", "secret")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored copied: %v", err)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(source, ".git", "index"))
	if string(indexBefore) != string(indexAfter) {
		t.Fatal("source index changed")
	}
	writeTest(t, source, "new.txt", "new-two\n")
	_, hash, err := r.Fingerprint(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if hash == a.ContentHash {
		t.Fatal("untracked rewrite not detected")
	}
	_, err = r.Capture(context.Background(), "first", source, "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("reuse must conflict: %v", err)
	}
	gitTest(t, source, "add", ".")
	gitTest(t, source, "commit", "--quiet", "-m", "same content")
	_, afterCommit, err := r.Fingerprint(context.Background(), source)
	if err != nil || afterCommit != hash {
		t.Fatalf("commit changed content identity: %s %s %v", hash, afterCommit, err)
	}
}

func TestCaptureLimitsSymlinksAndManagedDirectory(t *testing.T) {
	r, source := fixture(t)
	r.Root = filepath.Join(source, "managed-evals")
	a := captureTest(t, r, "inside", source)
	if _, hash, err := r.Fingerprint(context.Background(), source); err != nil || hash != a.ContentHash {
		t.Fatalf("managed data enters source: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(source, "escape")); err == nil {
		if _, _, err := r.Fingerprint(context.Background(), source); err == nil {
			t.Fatal("symlink accepted")
		}
		if err := os.Remove(filepath.Join(source, "escape")); err != nil {
			t.Fatal(err)
		}
	}
	r.Limits.MaxFileBytes = 2
	if _, _, err := r.Fingerprint(context.Background(), source); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err := r.Capture(context.Background(), "../outside", source, ""); err == nil {
		t.Fatal("unsafe id accepted")
	}
}

func helperSpec(t *testing.T, mode string, args ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := append([]string{exe, "-test.run=^TestEvalHelper$", "--", mode}, args...)
	return Spec{Command: command, Timeout: 3 * time.Second}
}
func TestEvalHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--" || i+1 >= len(os.Args) {
			continue
		}
		mode := os.Args[i+1]
		args := os.Args[i+2:]
		switch mode {
		case "cwd":
			cwd, _ := os.Getwd()
			fmt.Println(cwd)
			fmt.Println("SECRET=" + os.Getenv("SWARM_TEST_SECRET"))
			data, _ := os.ReadFile("a.txt")
			fmt.Print(string(data))
			_ = os.WriteFile("result.out", []byte("generated"), 0600)
		case "fail":
			fmt.Println("assertion failed")
			os.Exit(7)
		case "flood":
			fmt.Print(strings.Repeat("x", 10000))
		case "mutate":
			_ = os.WriteFile("a.txt", []byte("tampered"), 0600)
		case "sleep":
			time.Sleep(3 * time.Second)
		case "child":
			time.Sleep(500 * time.Millisecond)
			_ = os.WriteFile(args[0], []byte("escaped"), 0600)
		case "tree":
			exe, _ := os.Executable()
			cmd := exec.Command(exe, "-test.run=^TestEvalHelper$", "--", "child", args[0])
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Start()
			time.Sleep(3 * time.Second)
		default:
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func TestEvaluateUsesCopyBoundsOutputAndDoesNotLeakEnvironment(t *testing.T) {
	if !ProcessTreeSupported() {
		t.Skip("process tree unsupported")
	}
	r, source := fixture(t)
	a := captureTest(t, r, "candidate", source)
	t.Setenv("SWARM_TEST_SECRET", "must-not-leak")
	spec := helperSpec(t, "cwd")
	result, err := r.Evaluate(context.Background(), "run", a, spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusPassed || result.ExitCode != 0 || !strings.Contains(result.OutputHead, filepath.Join(r.Root, "evals", "run", "tree")) || strings.Contains(result.OutputHead, "must-not-leak") {
		t.Fatalf("result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(source, "result.out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("evaluation wrote source")
	}
	again, err := r.Evaluate(context.Background(), "run", a, spec)
	if err != nil || !again.StartedAt.Equal(result.StartedAt) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := r.Evaluate(context.Background(), "run", a, helperSpec(t, "fail")); !errors.Is(err, ErrConflict) {
		t.Fatalf("different spec: %v", err)
	}
	r.Limits.MaxOutputBytes = 100
	large, err := r.Evaluate(context.Background(), "large", a, helperSpec(t, "flood"))
	if err != nil || !large.Truncated || len(large.OutputHead) != 100 {
		t.Fatalf("bounded output: %#v %v", large, err)
	}
	failed, err := r.Evaluate(context.Background(), "failed", a, helperSpec(t, "fail"))
	if err != nil || failed.Status != StatusFailed || failed.ExitCode != 7 {
		t.Fatalf("failed: %#v %v", failed, err)
	}
	mutated, err := r.Evaluate(context.Background(), "mutated", a, helperSpec(t, "mutate"))
	if err != nil || mutated.Status != StatusInconclusive || mutated.Reason != "input_changed" {
		t.Fatalf("mutated: %#v %v", mutated, err)
	}
}

func TestEvaluationTimeoutKillsDescendantsAndRecoveryDoesNotRerun(t *testing.T) {
	if !ProcessTreeSupported() {
		t.Skip("process tree unsupported")
	}
	r, source := fixture(t)
	a := captureTest(t, r, "candidate", source)
	marker := filepath.Join(t.TempDir(), "child-survived")
	spec := helperSpec(t, "tree", marker)
	spec.Timeout = 150 * time.Millisecond
	result, err := r.Evaluate(context.Background(), "timeout", a, spec)
	if err != nil || result.Status != StatusInconclusive || result.Reason != "timeout" {
		t.Fatalf("timeout: %#v %v", result, err)
	}
	time.Sleep(650 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("descendant survived cancellation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Evaluate(ctx, "cancelled", a, helperSpec(t, "sleep")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
	dir, err := r.location("evals", "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Evaluate(context.Background(), "orphan", a, helperSpec(t, "cwd")); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan rerun: %v", err)
	}
}

func TestIntegrateIndependentChangesRejectConflictsAndStaleInputs(t *testing.T) {
	r, source := fixture(t)
	other := t.TempDir()
	gitTest(t, other, "clone", "--quiet", source, ".")
	writeTest(t, source, "a.txt", "new-a\n")
	writeTest(t, other, "b.txt", "new-b\n")
	a := captureTest(t, r, "a", source)
	b := captureTest(t, r, "b", other)
	i, err := r.Integrate(context.Background(), "combined", []*Artifact{a, b})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "new-a\n", "b.txt": "new-b\n"} {
		data, err := os.ReadFile(filepath.Join(i.Directory, name))
		if err != nil || string(data) != want {
			t.Fatalf("combined %s: %s %v", name, data, err)
		}
	}
	if _, err := r.Integrate(context.Background(), "duplicate", []*Artifact{a, a}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := r.Integrate(context.Background(), "lineage", []*Artifact{i, a}); !errors.Is(err, ErrConflict) {
		t.Fatalf("lineage duplicate: %v", err)
	}
	writeTest(t, other, "a.txt", "conflicting-a\n")
	c := captureTest(t, r, "c", other)
	if _, err := r.Integrate(context.Background(), "conflicting", []*Artifact{a, c}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := r.Integrate(context.Background(), "stale", []*Artifact{a, b}); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale source: %v", err)
	}
}

func TestSpecAndArtifactValidation(t *testing.T) {
	for _, spec := range []Spec{{Command: []string{"sh", "-c", "echo yes"}}, {Command: []string{"go", "test"}, Environment: map[string]string{"OPENAI_API_KEY": "secret"}}, {Command: []string{"go", "test"}, Timeout: time.Hour}} {
		if err := ValidateSpec(spec); err == nil {
			t.Fatalf("invalid spec accepted: %#v", spec)
		}
	}
	r, source := fixture(t)
	a := captureTest(t, r, "candidate", source)
	writeTest(t, a.Directory, "extra", "unexpected")
	if _, err := r.LoadArtifact(context.Background(), a.ID); !errors.Is(err, ErrChanged) {
		t.Fatalf("artifact injection: %v", err)
	}
}

func TestDetachedCandidateSurvivesSourceChanges(t *testing.T) {
	r, source := fixture(t)
	writeTest(t, source, "a.txt", "candidate-a\n")
	a := captureTest(t, r, "original", source)
	writeTest(t, source, "a.txt", "later-worker\n")
	detached, err := r.Detach(context.Background(), "frozen", a)
	if err != nil {
		t.Fatal(err)
	}
	if detached.SourceDir != "" || detached.ContentHash != a.ContentHash || len(detached.Inputs) != 1 || detached.Inputs[0] != a.ID {
		t.Fatalf("detach: %#v", detached)
	}
	i, err := r.Integrate(context.Background(), "integrated-frozen", []*Artifact{detached})
	if err != nil || i.ContentHash != a.ContentHash {
		t.Fatalf("integrate frozen: %#v %v", i, err)
	}
	if ProcessTreeSupported() {
		result, err := r.Evaluate(context.Background(), "frozen-eval", detached, helperSpec(t, "cwd"))
		if err != nil || result.Status != StatusPassed || !strings.Contains(result.OutputHead, "candidate-a") || strings.Contains(result.OutputHead, "later-worker") {
			t.Fatalf("frozen eval: %#v %v", result, err)
		}
	}
}
