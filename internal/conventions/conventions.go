// Package conventions owns the gateway's persisted result-visibility
// conventions (dataDir/conventions.json). The single switch — artifactPaths —
// makes phone-initiated turns inject a short output convention so agent
// artifacts stay visible on the phone instead of relying on the agent's own
// habits. Storage follows the jev/notifications precedents: ordered JSON,
// 0600, atomic tmp+rename writes.
package conventions

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// ArtifactPathHint is the convention text injected as the first extra block
// of a phone-initiated ACP turn: short, Chinese, and phrased so the reply's
// markdown links/images resolve into the /files and /desktop-images channels.
const ArtifactPathHint = "（网关约定）完成任务后，请在回复末尾用 Markdown 链接列出你产出或修改的文件路径；引用图片请使用 Markdown 图片语法（相对工作目录的路径或绝对路径），以便手机端预览和下载。"

// Settings is safe for concurrent HTTP reads and updates.
type Settings struct {
	path          string
	mu            sync.Mutex
	artifactPaths bool
}

func New(path string) *Settings {
	s := &Settings{path: filepath.Clean(path)}
	s.artifactPaths = s.load()
	return s
}

// load reads the stored switch; any absence or corruption falls back to the
// default (off) so a broken file never blocks sends.
func (s *Settings) load() bool {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return false
	}
	decoded, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return false
	}
	object, ok := decoded.(*jsoncompat.Object)
	if !ok {
		return false
	}
	enabled, _ := object.MustGet("artifactPaths").(bool)
	return enabled
}

// Public returns the client-safe settings view.
func (s *Settings) Public() *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publicLocked()
}

func (s *Settings) publicLocked() *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("artifactPaths", s.artifactPaths)
	return result
}

// ArtifactPaths reports whether phone-initiated turns should carry the
// artifact-path convention.
func (s *Settings) ArtifactPaths() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.artifactPaths
}

// Update validates and persists a settings form submission. Unknown keys are
// ignored; artifactPaths must be a boolean when present. The in-memory switch
// flips only after the disk write succeeded, so a failed save never leaves
// memory and file disagreeing.
func (s *Settings) Update(input *jsoncompat.Object) (*jsoncompat.Object, error) {
	if input == nil {
		return nil, errors.New("设置格式不正确")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	desired := s.artifactPaths
	if raw, exists := input.Get("artifactPaths"); exists {
		enabled, ok := raw.(bool)
		if !ok {
			return nil, errors.New("产物路径约定开关格式不正确")
		}
		desired = enabled
	}
	if err := s.saveLocked(desired); err != nil {
		return nil, err
	}
	s.artifactPaths = desired
	return s.publicLocked(), nil
}

// saveLocked writes atomically: tmp file + rename, 0600, trailing newline.
func (s *Settings) saveLocked(desired bool) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	stored := jsoncompat.NewObject()
	stored.Set("artifactPaths", desired)
	payload := append(jsoncompat.EncodePythonIndent2(stored), '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
