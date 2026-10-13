// Package uploads stores immutable phone attachments under the gateway data
// directory. Metadata and path checks mirror bridge/uploads.py.
package uploads

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

const (
	MaxFile  = 20 * 1024 * 1024
	MaxThumb = 1024 * 1024
)

var safeName = regexp.MustCompile(`^[^\\/\x00-\x1f\x7f]+$`)
var safeThread = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Manager struct {
	Root string
	mu   sync.Mutex
}

func New(dataDir string) *Manager { return &Manager{Root: filepath.Join(dataDir, "uploads")} }

func validate(thread, id, name string) error {
	if !safeThread.MatchString(thread) {
		return errors.New("会话标识无效")
	}
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("附件标识无效")
	}
	if name == "" || name == "." || name == ".." || len([]byte(name)) > 200 || !safeName.MatchString(name) {
		return errors.New("文件名无效")
	}
	return nil
}

func imageType(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff":
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func suffix(name, mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	ext := strings.ToLower(filepath.Ext(name))
	if regexp.MustCompile(`^\.[a-z0-9]{1,16}$`).MatchString(ext) {
		return ext
	}
	return ".bin"
}

func (m *Manager) folder(thread, id string) string { return filepath.Join(m.Root, thread, id) }
func (m *Manager) metaPath(thread, id string) string {
	return filepath.Join(m.Root, thread, id+".json")
}

func (m *Manager) Put(thread, id, name string, data []byte) (*jsoncompat.Object, error) {
	if err := validate(thread, id, name); err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxFile {
		return nil, errors.New("每个附件需为 1 字节至 20 MB")
	}
	digest := sha256.Sum256(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, err := m.Get(thread, id); err == nil {
		if existing.MustGet("sha256") != hex.EncodeToString(digest[:]) || existing.MustGet("name") != name {
			return nil, errors.New("同一附件标识不能用于不同文件")
		}
		return public(existing), nil
	}
	dir := m.folder(thread, id)
	internal := filepath.Join(dir, "internal")
	if err := os.MkdirAll(internal, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(internal, "original"+suffix(name, imageType(data)))
	temporary := filepath.Join(internal, "original.part")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return nil, err
	}
	row := jsoncompat.NewObject()
	row.Set("id", id)
	row.Set("name", name)
	row.Set("size", jsoncompat.Num(len(data)))
	row.Set("sha256", hex.EncodeToString(digest[:]))
	row.Set("createdAt", time.Now().Unix())
	row.Set("image", imageType(data))
	row.Set("localPath", path)
	if err := m.writeMeta(thread, id, row); err != nil {
		return nil, err
	}
	return public(row), nil
}

func (m *Manager) Get(thread, id string) (*jsoncompat.Object, error) {
	if !safeThread.MatchString(thread) {
		return nil, errors.New("会话标识无效")
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, errors.New("附件标识无效")
	}
	raw, err := os.ReadFile(m.metaPath(thread, id))
	if err != nil {
		return nil, errors.New("附件已不可用，请重新上传")
	}
	value, err := jsoncompat.Unmarshal(raw)
	row, _ := value.(*jsoncompat.Object)
	if err != nil || row == nil {
		return nil, errors.New("附件已不可用，请重新上传")
	}
	path, _ := row.MustGet("localPath").(string)
	resolved, err := filepath.Abs(path)
	if err != nil || !within(filepath.Join(m.folder(thread, id), "internal"), resolved) {
		return nil, errors.New("附件路径无效")
	}
	if info, statErr := os.Stat(resolved); statErr != nil || !info.Mode().IsRegular() {
		return nil, errors.New("附件已不可用，请重新上传")
	}
	return row, nil
}

func (m *Manager) Preview(thread, id, variant string) (*jsoncompat.Object, string, error) {
	row, err := m.Get(thread, id)
	if err != nil {
		return nil, "", err
	}
	image, _ := row.MustGet("image").(string)
	if image == "" {
		return nil, "", errors.New("图片附件不存在")
	}
	if variant != "original" {
		if thumb, ok := row.Get("thumbPath"); ok {
			name, _ := thumb.(string)
			path := filepath.Join(m.folder(thread, id), "internal", name)
			if within(filepath.Join(m.folder(thread, id), "internal"), path) {
				if _, statErr := os.Stat(path); statErr == nil {
					result := jsoncompat.Clone(row).(*jsoncompat.Object)
					result.Set("previewPath", path)
					result.Set("previewMime", row.MustGet("thumbMime"))
					result.Set("previewSha256", row.MustGet("thumbSha256"))
					return result, "thumb", nil
				}
			}
		}
	}
	result := jsoncompat.Clone(row).(*jsoncompat.Object)
	result.Set("previewPath", row.MustGet("localPath"))
	result.Set("previewMime", image)
	result.Set("previewSha256", row.MustGet("sha256"))
	if variant == "original" {
		return result, "original", nil
	}
	return result, "fallback-original", nil
}

func (m *Manager) SetThumb(thread, id string, data []byte, width, height int) (*jsoncompat.Object, error) {
	if len(data) == 0 || len(data) > MaxThumb {
		return nil, errors.New("缩略图需为 1 字节至 1 MB")
	}
	if width < 1 || width > 8192 || height < 1 || height > 8192 {
		return nil, errors.New("缩略图尺寸无效")
	}
	row, err := m.Get(thread, id)
	if err != nil {
		return nil, err
	}
	if !jsoncompat.Truthy(row.MustGet("image")) {
		return nil, errors.New("只有图片附件可以生成缩略图")
	}
	mime := imageType(data)
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
		return nil, errors.New("缩略图格式无效")
	}
	ext := strings.TrimPrefix(mime, "image/")
	if ext == "jpeg" {
		ext = "jpg"
	}
	path := filepath.Join(m.folder(thread, id), "internal", "thumb."+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	row.Set("thumb", mime)
	row.Set("thumbPath", filepath.Base(path))
	row.Set("thumbMime", mime)
	row.Set("thumbWidth", jsoncompat.Num(width))
	row.Set("thumbHeight", jsoncompat.Num(height))
	row.Set("thumbSha256", hex.EncodeToString(digest[:]))
	if err := m.writeMeta(thread, id, row); err != nil {
		return nil, err
	}
	return public(row), nil
}

func (m *Manager) writeMeta(thread, id string, row *jsoncompat.Object) error {
	if err := os.MkdirAll(filepath.Dir(m.metaPath(thread, id)), 0o700); err != nil {
		return err
	}
	tmp := m.metaPath(thread, id) + ".tmp"
	if err := os.WriteFile(tmp, append(jsoncompat.EncodePythonIndent2(row), '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.metaPath(thread, id))
}

func public(row *jsoncompat.Object) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for _, key := range []string{"id", "name", "size", "image", "thumb", "thumbWidth", "thumbHeight"} {
		if value, ok := row.Get(key); ok {
			result.Set(key, value)
		}
	}
	return result
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
