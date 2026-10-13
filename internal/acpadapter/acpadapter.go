// Package acpadapter ports bridge/agents/acp_adapter.py's dialect-shared
// decision slice: attachment block construction (capability gating, magic
// sniffing, base64, file URIs) and work-mode switching. The lifecycle and
// queue machinery lands with the Go bridge (P6); parity for this slice is
// enforced against conformance/acpadapter goldens.
package acpadapter

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// ImageType mirrors bridge/uploads.py image_type magic sniffing.
func ImageType(data []byte) string {
	switch {
	case hasPrefix(data, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case hasPrefix(data, "\xff\xd8\xff"):
		return "image/jpeg"
	case hasPrefix(data, "GIF87a"), hasPrefix(data, "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func hasPrefix(data []byte, prefix string) bool {
	return len(data) >= len(prefix) && string(data[:len(prefix)]) == prefix
}

// FileSpec is one attachment the phone uploaded.
type FileSpec struct {
	Image bool
	Name  string
	Path  string
}

// AttachmentBlocks mirrors AcpAdapter._attachment_blocks.
func AttachmentBlocks(capabilities *jsoncompat.Object, files []FileSpec) (*jsoncompat.Array, error) {
	promptCaps := jsoncompat.NewObject()
	if capabilities != nil {
		if value, ok := capabilities.MustGet("promptCapabilities").(*jsoncompat.Object); ok {
			promptCaps = value
		}
	}
	blocks := &jsoncompat.Array{}
	for _, item := range files {
		if item.Image {
			if supported, _ := promptCaps.MustGet("image").(bool); !supported {
				return nil, errors.New("此 Agent 暂不支持图片附件")
			}
			data, err := os.ReadFile(item.Path)
			if err != nil {
				return nil, err
			}
			mimeType := ImageType(data)
			if mimeType == "" {
				mimeType = "image/png"
			}
			block := jsoncompat.NewObject()
			block.Set("type", "image")
			block.Set("data", base64.StdEncoding.EncodeToString(data))
			block.Set("mimeType", mimeType)
			blocks.Items = append(blocks.Items, block)
		} else {
			if supported, _ := promptCaps.MustGet("embeddedContext").(bool); !supported {
				return nil, errors.New("此 Agent 暂不支持文件附件")
			}
			block := jsoncompat.NewObject()
			block.Set("type", "resource_link")
			block.Set("name", item.Name)
			block.Set("uri", FileURI(item.Path))
			blocks.Items = append(blocks.Items, block)
		}
	}
	return blocks, nil
}

// FileURI mirrors Path.resolve().as_uri(): symlink-resolved absolute path,
// percent-encoded with Python's urllib.parse.quote safe set.
func FileURI(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	var builder strings.Builder
	builder.WriteString("file://")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c >= 0x80 || !isQuoteSafe(c) {
			const hexDigits = "0123456789ABCDEF"
			builder.WriteByte('%')
			builder.WriteByte(hexDigits[c>>4])
			builder.WriteByte(hexDigits[c&0xf])
		} else {
			builder.WriteByte(c)
		}
	}
	return builder.String()
}

// isQuoteSafe mirrors urllib.parse.quote's default safe characters
// (unreserved set plus '/'), with lowercase-hex output.
func isQuoteSafe(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	switch c {
	case '_', '.', '-', '~', '/':
		return true
	}
	return false
}

// SetMode mirrors AcpAdapter._set_mode: availability check first (even a
// current mode fails when availableModes is missing), skip when already
// current, else session/set_mode and bookkeeping.
func SetMode(sessionID, modeID string, modes *jsoncompat.Object,
	request func(params *jsoncompat.Object), view *acpview.View) error {
	available := map[string]bool{}
	if modes != nil {
		if list, ok := modes.MustGet("availableModes").(*jsoncompat.Array); ok {
			for _, item := range list.Items {
				if entry, isObject := item.(*jsoncompat.Object); isObject {
					if id, isString := entry.MustGet("id").(string); isString {
						available[id] = true
					}
				}
			}
		}
	}
	if !available[modeID] {
		return errors.New("当前 Agent 不支持此工作模式")
	}
	current := ""
	if modes != nil {
		current, _ = modes.MustGet("currentModeId").(string)
	}
	if current == modeID {
		return nil
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", sessionID)
	params.Set("modeId", modeID)
	request(params)
	modes.Set("currentModeId", modeID)
	view.SetMode(modeID)
	return nil
}
