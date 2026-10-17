package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestSettingsDefaultOffAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conventions.json")
	settings := New(path)
	if settings.ArtifactPaths() {
		t.Fatal("fresh settings must default to off")
	}
	if enabled, _ := settings.Public().MustGet("artifactPaths").(bool); enabled {
		t.Fatalf("public view wrong: %s", settings.Public())
	}

	updated, err := settings.Update(jsonObject(t, `{"artifactPaths":true,"unknown":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	if updated.MustGet("artifactPaths") != true {
		t.Fatalf("update result wrong: %s", updated)
	}
	if !settings.ArtifactPaths() {
		t.Fatal("in-memory switch not flipped")
	}

	// A fresh instance reads the persisted file: the write really landed.
	reloaded := New(path)
	if !reloaded.ArtifactPaths() {
		t.Fatal("persistence lost across instances")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored file mode=%v err=%v", info.Mode(), err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind after rename")
	}

	// Missing key keeps the current value; flipping back off persists too.
	if _, err := settings.Update(jsonObject(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	if !settings.ArtifactPaths() {
		t.Fatal("empty update must not reset the switch")
	}
	if _, err := settings.Update(jsonObject(t, `{"artifactPaths":false}`)); err != nil {
		t.Fatal(err)
	}
	if New(path).ArtifactPaths() {
		t.Fatal("disable did not persist")
	}
}

func TestSettingsValidationAndCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conventions.json")
	settings := New(path)
	if _, err := settings.Update(jsonObject(t, `{"artifactPaths":"yes"}`)); err == nil {
		t.Fatal("non-boolean artifactPaths must be rejected")
	}
	if settings.ArtifactPaths() {
		t.Fatal("rejected update must not flip the switch")
	}
	if _, err := settings.Update(nil); err == nil {
		t.Fatal("nil input must be rejected")
	}

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if New(path).ArtifactPaths() {
		t.Fatal("corrupt file must fall back to off")
	}
}

func TestArtifactPathHintContent(t *testing.T) {
	// The injected convention must stay a short Chinese hint naming the two
	// behaviors the phone preview channels rely on.
	if !strings.Contains(ArtifactPathHint, "文件路径") || !strings.Contains(ArtifactPathHint, "Markdown 图片语法") {
		t.Fatalf("hint lost its key requests: %s", ArtifactPathHint)
	}
	if len([]rune(ArtifactPathHint)) > 160 {
		t.Fatalf("hint too long: %d runes", len([]rune(ArtifactPathHint)))
	}
}

func jsonObject(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}
