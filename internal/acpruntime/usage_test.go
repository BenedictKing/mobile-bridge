package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeFakeAgent(t *testing.T, script string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}
