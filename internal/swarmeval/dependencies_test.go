package swarmeval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDependencyCommandSelectionUsesExistingLocks(t *testing.T) {
	for _, test := range []struct {
		name, lock, manager string
		want                []string
	}{{"npm", "package-lock.json", "npm@11.0.0", []string{"npm", "ci", "--no-audit", "--no-fund"}}, {"pnpm", "pnpm-lock.yaml", "pnpm@10.0.0", []string{"pnpm", "install", "--frozen-lockfile"}}, {"yarn", "yarn.lock", "yarn@4.0.0", []string{"yarn", "install", "--immutable"}}} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTest(t, dir, "package.json", `{"packageManager":"`+test.manager+`","dependencies":{"example":"1.0.0"}}`)
			writeTest(t, dir, test.lock, "frozen-lock")
			commands, reason, err := dependencyCommands(dir)
			if err != nil || reason != "" || !reflect.DeepEqual(commands, [][]string{test.want}) {
				t.Fatalf("selection: %v %s %v", commands, reason, err)
			}
		})
	}
	dir := t.TempDir()
	writeTest(t, dir, "go.mod", "module example.invalid/test\nrequire example.invalid/dependency v1.0.0\n")
	writeTest(t, dir, "go.sum", "frozen sums")
	commands, reason, err := dependencyCommands(dir)
	if err != nil || reason != "" || !reflect.DeepEqual(commands, [][]string{{"go", "mod", "download"}}) {
		t.Fatalf("go selection: %v %s %v", commands, reason, err)
	}
}

func fakePreparationTool(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "npm"), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPrepareDependenciesMissingLockIsEnvironmentFailure(t *testing.T) {
	if !ProcessTreeSupported() {
		t.Skip("process trees unsupported")
	}
	r, source := fixture(t)
	writeTest(t, source, "package.json", `{"dependencies":{"example":"1.0.0"}}`)
	a := captureTest(t, r, "missing-lock", source)
	spec := helperSpec(t, "cwd")
	spec.PrepareDependencies = true
	result, err := r.Evaluate(context.Background(), "lock-absent", a, spec)
	if err != nil || result.Status != StatusInconclusive || result.Reason != "lockfile_missing" || result.ProcessID != 0 {
		t.Fatalf("missing lock: %#v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "evals", "lock-absent", "tree", "result.out")); !os.IsNotExist(err) {
		t.Fatal("business command ran after missing lock")
	}
	spec.PrepareDependencies = false
	withoutSetup, err := r.Evaluate(context.Background(), "no-preflight", a, spec)
	if err != nil || withoutSetup.Status != StatusPassed {
		t.Fatalf("default behavior changed: %#v %v", withoutSetup, err)
	}
}

func TestPrepareDependenciesUsesIsolatedTreeAndFrozenLock(t *testing.T) {
	if !ProcessTreeSupported() {
		t.Skip("process trees unsupported")
	}
	r, source := fixture(t)
	writeTest(t, source, "package.json", `{"dependencies":{"example":"1.0.0"}}`)
	writeTest(t, source, "package-lock.json", "frozen\n")
	a := captureTest(t, r, "locked", source)
	fake := fakePreparationTool(t, `printf '%s\n' "$*" "$PWD" "$HOME" "$npm_config_cache"; printf prepared > dependency-ready`)
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	spec := helperSpec(t, "cwd")
	spec.PrepareDependencies = true
	result, err := r.Evaluate(context.Background(), "prepared", a, spec)
	if err != nil || result.Status != StatusPassed || len(result.Preparation) != 1 || result.Preparation[0].ExitCode != 0 {
		t.Fatalf("prepared: %#v %v", result, err)
	}
	if !strings.Contains(result.OutputHead, "ci --no-audit --no-fund") || !strings.Contains(result.OutputHead, filepath.Join(r.Root, "evals", "prepared", "cache", "npm")) {
		t.Fatalf("fixed argv/cache absent: %s", result.OutputHead)
	}
	if _, err := os.Stat(filepath.Join(source, "dependency-ready")); !os.IsNotExist(err) {
		t.Fatal("dependency prepare mutated source")
	}
	if _, err := os.Stat(filepath.Join(r.Root, "evals", "prepared", "tree", "dependency-ready")); err != nil {
		t.Fatal("prepare did not run in eval tree")
	}
}

func TestPrepareDependencyFailuresAreInconclusiveAndWithinTimeout(t *testing.T) {
	if !ProcessTreeSupported() {
		t.Skip("process trees unsupported")
	}
	for _, test := range []struct {
		name, script, reason string
		timeout              time.Duration
	}{{"install-failed", "printf network-unavailable; exit 9", "environment_setup_failed", 3 * time.Second}, {"lock-modified", "printf changed > package-lock.json", "input_changed", 3 * time.Second}, {"timeout", "while :; do :; done", "timeout", 100 * time.Millisecond}} {
		t.Run(test.name, func(t *testing.T) {
			r, source := fixture(t)
			writeTest(t, source, "package.json", `{"dependencies":{"example":"1.0.0"}}`)
			writeTest(t, source, "package-lock.json", "frozen\n")
			a := captureTest(t, r, "candidate", source)
			t.Setenv("PATH", fakePreparationTool(t, test.script)+string(os.PathListSeparator)+os.Getenv("PATH"))
			spec := helperSpec(t, "cwd")
			spec.PrepareDependencies = true
			spec.Timeout = test.timeout
			result, err := r.Evaluate(context.Background(), "prepare", a, spec)
			if err != nil || result.Status != StatusInconclusive || result.Reason != test.reason || result.ProcessID != 0 {
				t.Fatalf("prepare failure: %#v %v", result, err)
			}
		})
	}
}

func TestDependencyLockMismatchAndGoMissingSum(t *testing.T) {
	dir := t.TempDir()
	writeTest(t, dir, "package.json", `{"packageManager":"pnpm@10","dependencies":{"example":"1"}}`)
	writeTest(t, dir, "package-lock.json", "frozen")
	if _, reason, err := dependencyCommands(dir); err == nil || reason != "lockfile_conflict" {
		t.Fatalf("mismatch: %s %v", reason, err)
	}
	goDir := t.TempDir()
	writeTest(t, goDir, "go.mod", "module example.invalid/test\nrequire (\nexample.invalid/dependency v1.0.0\n)\n")
	if _, reason, err := dependencyCommands(goDir); err == nil || reason != "lockfile_missing" {
		t.Fatalf("go sum: %s %v", reason, err)
	}
}

func TestPrepareDependenciesMissingToolAndParentCancellation(t *testing.T) {
	if !ProcessTreeSupported() {
		t.Skip("process trees unsupported")
	}
	r, source := fixture(t)
	writeTest(t, source, "package.json", `{"dependencies":{"example":"1.0.0"}}`)
	writeTest(t, source, "package-lock.json", "frozen\n")
	captured := captureTest(t, r, "candidate", source)
	a, err := r.Detach(context.Background(), "detached", captured)
	if err != nil {
		t.Fatal(err)
	}
	// detached 制品的执行不需源 Git，空 PATH 可确定验证工具缺失且不触发真实网络。
	t.Setenv("PATH", t.TempDir())
	spec := helperSpec(t, "cwd")
	spec.PrepareDependencies = true
	result, err := r.Evaluate(context.Background(), "missing-tool", a, spec)
	if err != nil || result.Status != StatusInconclusive || result.Reason != "environment_setup_failed" || result.ProcessID != 0 {
		t.Fatalf("missing tool: %#v %v", result, err)
	}
	t.Setenv("PATH", fakePreparationTool(t, "while :; do :; done"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err = r.Evaluate(ctx, "parent-cancelled", a, spec)
	if err != nil || result.Status != StatusInterrupted || result.Reason != "cancelled" || result.ProcessID != 0 {
		t.Fatalf("parent cancelled: %#v %v", result, err)
	}
}
