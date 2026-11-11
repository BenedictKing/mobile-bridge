// Package agentmedia turns inline agent images into bounded local files shared
// by live ACP updates and native transcript readers.
package agentmedia

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const maxImageBytes = 20 * 1024 * 1024

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// PersistImages rewrites structured image blocks in place. Image data is never
// retained in a view, including when decoding or persistence fails.
func PersistImages(content any, dir, sessionID string) {
	switch value := content.(type) {
	case *jsoncompat.Array:
		for _, block := range value.Items {
			PersistImages(block, dir, sessionID)
		}
	case *jsoncompat.Object:
		if value.MustGet("type") == "image" {
			persistImage(value, dir, sessionID)
			return
		}
		for _, key := range []string{"content", "message", "data"} {
			if nested, ok := value.Get(key); ok {
				PersistImages(nested, dir, sessionID)
			}
		}
	}
}

func persistImage(block *jsoncompat.Object, dir, sessionID string) {
	data, _ := block.MustGet("data").(string)
	mime, _ := block.MustGet("mimeType").(string)
	defer block.Delete("data")
	if source, _ := block.MustGet("source").(*jsoncompat.Object); source != nil {
		defer source.Delete("data")
		if source.MustGet("type") == "base64" {
			data, _ = source.MustGet("data").(string)
			mime, _ = source.MustGet("media_type").(string)
		}
	}
	if data == "" || dir == "" || !safeSessionID.MatchString(sessionID) {
		return
	}
	if strings.HasPrefix(data, "data:image/") {
		header, body, ok := strings.Cut(data, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return
		}
		mime, data = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"), body
	}
	if len(data) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) == 0 || len(decoded) > maxImageBytes {
		return
	}
	sum := sha256.Sum256(decoded)
	path := filepath.Join(dir, sessionID, hex.EncodeToString(sum[:12])+imageSuffix(mime, decoded))
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		block.Set("path", path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".image-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(decoded)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Rename(file.Name(), path) != nil {
		return
	}
	block.Set("path", path)
}

func imageSuffix(mime string, data []byte) string {
	switch {
	case strings.HasSuffix(mime, "png"), len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png"
	case strings.HasSuffix(mime, "jpeg"), len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff":
		return ".jpg"
	case strings.HasSuffix(mime, "gif"), len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return ".gif"
	case strings.HasSuffix(mime, "webp"), len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	return ".bin"
}
