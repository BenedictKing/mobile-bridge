// Inline image block persistence: ACP agents may stream image content as
// base64 data. Decoding them to per-session files keeps view snapshots small
// (the alternative is cloning megabytes on every SSE poll) and turns each
// block into a local path the timeline attachment annotation can serve.
package acpruntime

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

// maxMediaBlock bounds one decoded image block, matching the upload limit.
const maxMediaBlock = 20 * 1024 * 1024

var safeMediaDir = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// prepareMedia rewrites image blocks with inline data in one session/update
// payload into {type: image, path} references. Failures degrade to dropping
// the data field — the block then behaves like the pre-media era (no
// preview, no base64 dumped into tool output text).
func (s *Session) prepareMedia(update *jsoncompat.Object) {
	if s.mediaDir == "" || update == nil {
		return
	}
	kind, _ := update.MustGet("sessionUpdate").(string)
	var blocks []*jsoncompat.Object
	switch kind {
	case "agent_message_chunk", "agent_thought_chunk":
		if block, _ := update.MustGet("content").(*jsoncompat.Object); block != nil {
			blocks = []*jsoncompat.Object{block}
		}
	case "tool_call", "tool_call_update":
		if list, _ := update.MustGet("content").(*jsoncompat.Array); list != nil {
			for _, item := range list.Items {
				if block, _ := item.(*jsoncompat.Object); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
	}
	for _, block := range blocks {
		s.persistImageBlock(block)
	}
}

func (s *Session) persistImageBlock(block *jsoncompat.Object) {
	if blockType, _ := block.MustGet("type").(string); blockType != "image" {
		return
	}
	data, _ := block.MustGet("data").(string)
	if data == "" {
		return // already a path reference, or nothing to persist
	}
	// The base64 payload never reaches the view either way: without a
	// persisted copy it would only resurface as truncated JSON in tool
	// output text.
	defer block.Delete("data")
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) == 0 || len(decoded) > maxMediaBlock {
		return
	}
	if !safeMediaDir.MatchString(s.ID) {
		return
	}
	sum := sha256.Sum256(decoded)
	name := hex.EncodeToString(sum[:12]) + mediaSuffix(block.MustGet("mimeType"), decoded)
	path := filepath.Join(s.mediaDir, s.ID, name)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		block.Set("path", path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	temporary := path + ".part"
	if err := os.WriteFile(temporary, decoded, 0o600); err != nil {
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return
	}
	block.Set("path", path)
}

func mediaSuffix(mime any, data []byte) string {
	mimeType, _ := mime.(string)
	switch {
	case strings.HasSuffix(mimeType, "png"):
		return ".png"
	case strings.HasSuffix(mimeType, "jpeg"):
		return ".jpg"
	case strings.HasSuffix(mimeType, "gif"):
		return ".gif"
	case strings.HasSuffix(mimeType, "webp"):
		return ".webp"
	}
	// Unknown or missing mime: fall back to the magic bytes.
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png"
	case len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff":
		return ".jpg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return ".gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	return ".bin"
}
