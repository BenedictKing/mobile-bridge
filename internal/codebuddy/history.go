package codebuddy

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
)

const (
	historyMaxBytes     = 256 << 20
	historyMaxLineBytes = 16 << 20
	historyStaleAfter   = 15 * time.Minute
)

type historyReader struct {
	index      *transcriptIndex
	mu         sync.Mutex
	signatures map[string]string
	pending    map[string]bool
}

// TranscriptReader loads CodeBuddy's message transcripts without starting the
// CLI. Newer meta-only copies never hide a copy that contains real turns.
func TranscriptReader(home string) func(string) (*acpview.NativeSnapshot, error) {
	reader := &historyReader{index: &transcriptIndex{root: ProjectsDir(home)},
		signatures: map[string]string{}, pending: map[string]bool{}}
	return reader.load
}

func (r *historyReader) load(sessionID string) (*acpview.NativeSnapshot, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return nil, nil
	}
	paths := r.index.resolve(sessionID)
	sort.SliceStable(paths, func(i, j int) bool {
		left, leftErr := os.Stat(paths[i])
		right, rightErr := os.Stat(paths[j])
		if leftErr != nil || rightErr != nil {
			return rightErr != nil
		}
		return left.ModTime().After(right.ModTime())
	})
	var selected *acpview.NativeSnapshot
	var selectedSignature string
	var selectedPending bool
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		signature := path + "|" + jsoncompat.PyStr(info.Size()) + "|" + jsoncompat.PyStr(info.ModTime().UnixMilli())
		r.mu.Lock()
		previous := r.signatures[sessionID]
		pending := r.pending[sessionID]
		r.mu.Unlock()
		if previous == signature && !pending {
			return nil, nil
		}
		stale := previous == signature && pending && time.Since(info.ModTime()) >= historyStaleAfter
		snapshot, err := readHistory(path, stale)
		if err != nil || snapshot == nil {
			continue
		}
		selected = snapshot
		selectedSignature = signature
		selectedPending = nativeHistoryActive(snapshot)
	}
	if selected == nil {
		return nil, nil
	}
	r.mu.Lock()
	r.signatures[sessionID] = selectedSignature
	r.pending[sessionID] = selectedPending
	r.mu.Unlock()
	return selected, nil
}

func readHistory(path string, stale bool) (*acpview.NativeSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer file.Close()
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	builder := acpview.NewNativeBuilder(sessionID)
	scanner := bufio.NewScanner(io.LimitReader(file, historyMaxBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), historyMaxLineBytes)
	for scanner.Scan() {
		value, err := jsoncompat.Unmarshal(scanner.Bytes())
		if err != nil {
			continue
		}
		event, _ := value.(*jsoncompat.Object)
		if event == nil {
			continue
		}
		kind, _ := event.MustGet("type").(string)
		if kind == "ai-title" {
			if title, _ := event.MustGet("aiTitle").(string); title != "" {
				builder.SetTitle(title)
			}
			continue
		}
		if kind != "message" {
			continue
		}
		role, _ := event.MustGet("role").(string)
		content := event.MustGet("content")
		titleText := ""
		switch role {
		case "user":
			titleText = applyUser(builder, content)
		case "assistant":
			applyAssistant(builder, content)
		}
		if builder.HasTurns() && builder.Title() == "" {
			if title := textutil.TitleFromText(titleText); title != "" {
				builder.SetTitle(title)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return builder.Snapshot(stale), nil
}

func applyUser(builder *acpview.NativeBuilder, content any) string {
	blocks, _ := content.(*jsoncompat.Array)
	if blocks == nil {
		if text, ok := content.(string); ok {
			builder.User(text)
			return text
		}
		return ""
	}
	hasToolResult := false
	for _, item := range blocks.Items {
		block, _ := item.(*jsoncompat.Object)
		if block == nil || block.MustGet("type") != "tool_result" {
			continue
		}
		hasToolResult = true
		id, _ := block.MustGet("tool_use_id").(string)
		failed, _ := block.MustGet("is_error").(bool)
		builder.ToolResult(id, block.MustGet("content"), failed)
	}
	if hasToolResult {
		return ""
	}
	text := acpview.Text(blocks)
	builder.User(text)
	return text
}

func applyAssistant(builder *acpview.NativeBuilder, content any) {
	blocks, _ := content.(*jsoncompat.Array)
	if blocks == nil {
		return
	}
	for _, item := range blocks.Items {
		block, _ := item.(*jsoncompat.Object)
		if block == nil {
			continue
		}
		switch block.MustGet("type") {
		case "text", "output_text":
			text, _ := block.MustGet("text").(string)
			builder.Agent(text)
		case "thinking":
			text, _ := block.MustGet("thinking").(string)
			builder.Thought(text)
		case "tool_use":
			id, _ := block.MustGet("id").(string)
			name, _ := block.MustGet("name").(string)
			builder.Tool(id, name, block.MustGet("input"))
		}
	}
}

func nativeHistoryActive(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}
