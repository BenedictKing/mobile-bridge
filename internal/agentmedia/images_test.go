package agentmedia

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestPersistenceFailureNeverRetainsInlineImageData(t *testing.T) {
	for _, scenario := range []struct{ name, dir, id, data string }{
		{"disabled", "", "session", "aGVsbG8="},
		{"invalid", t.TempDir(), "session", "not-base64!"},
		{"unsafeSession", t.TempDir(), "../escape", "aGVsbG8="},
		{"oversized", t.TempDir(), "session", strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)+4)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			source := jsoncompat.NewObject()
			source.Set("type", "base64")
			source.Set("data", scenario.data)
			block := jsoncompat.NewObject()
			block.Set("type", "image")
			block.Set("source", source)
			PersistImages(block, scenario.dir, scenario.id)
			if _, exists := source.Get("data"); exists || block.MustGet("path") != nil {
				t.Fatal("失败路径不得保留编码或生成可预览附件")
			}
		})
	}
}

func TestConcurrentIdenticalImagesUseCompleteContentAddressedFiles(t *testing.T) {
	dir := t.TempDir()
	data := []byte("\x89PNG\r\n\x1a\nconcurrent-image")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			block := jsoncompat.NewObject()
			block.Set("type", "image")
			block.Set("data", base64.StdEncoding.EncodeToString(data))
			PersistImages(block, dir, "session")
			path, _ := block.MustGet("path").(string)
			if raw, err := os.ReadFile(path); err != nil || string(raw) != string(data) {
				t.Errorf("并发落盘不能产生不完整文件: %v", err)
			}
		}()
	}
	wg.Wait()
	if entries, err := os.ReadDir(filepath.Join(dir, "session")); err != nil || len(entries) != 1 {
		t.Fatalf("同图应去重且不残留临时文件: %v", err)
	}
}
