package claude

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
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
	signatures map[string][2]int64
	pending    map[string]bool
}

// TranscriptReader loads Claude's persisted main-chain events without starting
// claude-agent-acp. A nil snapshot means the file is unchanged or has no turns.
func TranscriptReader(root string) func(string) (*acpview.NativeSnapshot, error) {
	reader := &historyReader{index: &transcriptIndex{root: root},
		signatures: map[string][2]int64{}, pending: map[string]bool{}}
	return reader.load
}

func (r *historyReader) load(sessionID string) (*acpview.NativeSnapshot, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return nil, nil
	}
	path := r.index.resolve(sessionID)
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	signature := [2]int64{info.Size(), info.ModTime().UnixMilli()}
	r.mu.Lock()
	previous, seen := r.signatures[sessionID]
	pending := r.pending[sessionID]
	r.mu.Unlock()
	stale := false
	if seen && previous == signature {
		if !pending || time.Since(info.ModTime()) < historyStaleAfter {
			return nil, nil
		}
		stale = true
	}
	snapshot, _, err := readHistory(path, stale)
	if err != nil || snapshot == nil {
		return nil, err
	}
	r.mu.Lock()
	r.signatures[sessionID] = signature
	r.pending[sessionID] = nativeHistoryActive(snapshot)
	r.mu.Unlock()
	return snapshot, nil
}

// ListSessions enumerates native Claude transcripts as ACP session/list rows.
func ListSessions(root string) []*jsoncompat.Object {
	index := &transcriptIndex{root: root}
	index.rebuild()
	result := []*jsoncompat.Object{}
	for sessionID, path := range index.files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		snapshot, cwd, err := readHistory(path, time.Since(info.ModTime()) >= historyStaleAfter)
		if err != nil || snapshot == nil || cwd == "" {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", sessionID)
		if snapshot.Title != "" {
			row.Set("title", snapshot.Title)
		}
		row.Set("cwd", cwd)
		row.Set("updatedAt", jsoncompat.Num(int(info.ModTime().UnixMilli())))
		result = append(result, row)
	}
	return result
}

func readHistory(path string, stale bool) (*acpview.NativeSnapshot, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", nil
	}
	defer file.Close()
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	builder := acpview.NewNativeBuilder(sessionID)
	cwd := ""
	scanner := bufio.NewScanner(io.LimitReader(file, historyMaxBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), historyMaxLineBytes)
	for scanner.Scan() {
		value, err := jsoncompat.Unmarshal(scanner.Bytes())
		if err != nil {
			continue
		}
		event, _ := value.(*jsoncompat.Object)
		if event == nil || jsoncompat.Truthy(event.MustGet("isSidechain")) || jsoncompat.Truthy(event.MustGet("isMeta")) || event.MustGet("parent_tool_use_id") != nil {
			continue
		}
		if current, _ := event.MustGet("cwd").(string); current != "" {
			cwd = current
		}
		kind, _ := event.MustGet("type").(string)
		if kind == "summary" {
			if title, _ := event.MustGet("summary").(string); title != "" {
				builder.SetTitle(title)
			}
			continue
		}
		message, _ := event.MustGet("message").(*jsoncompat.Object)
		if message == nil {
			continue
		}
		content := message.MustGet("content")
		titleText := ""
		switch kind {
		case "user":
			titleText = applyClaudeUser(builder, content)
		case "assistant":
			applyClaudeAssistant(builder, message, content)
		}
		if builder.HasTurns() && builder.Title() == "" {
			if title := textutil.TitleFromText(titleText); title != "" {
				builder.SetTitle(title)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	return builder.Snapshot(stale), cwd, nil
}

func nativeHistoryActive(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}

func applyClaudeUser(builder *acpview.NativeBuilder, content any) string {
	if text, ok := content.(string); ok {
		return foldUserText(builder, text)
	}
	blocks, _ := content.(*jsoncompat.Array)
	if blocks == nil {
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
	return foldUserText(builder, acpview.Text(blocks))
}

// foldUserText handles one user-side text. Claude Code echoes a user
// interrupt as a literal user record ("[Request interrupted by user]" or its
// tool-use variant) with no turn-end after it; folding it as a normal prompt
// leaves a forever-inProgress turn that keeps the phone read-only. The
// interrupted turn settles as interrupted and the marker itself renders as a
// closed turn instead.
func foldUserText(builder *acpview.NativeBuilder, text string) string {
	if strings.HasPrefix(text, "[Request interrupted by user") {
		builder.End("cancelled")
		builder.User(text)
		builder.End("cancelled")
		return ""
	}
	builder.User(text)
	return text
}

func applyClaudeAssistant(builder *acpview.NativeBuilder, message *jsoncompat.Object, content any) {
	if model, _ := message.MustGet("model").(string); model != "" {
		builder.SetModel(model)
	}
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
		case "text":
			text, _ := block.MustGet("text").(string)
			builder.Agent(text)
		case "thinking":
			text, _ := block.MustGet("thinking").(string)
			builder.Thought(text)
		case "tool_use", "server_tool_use", "mcp_tool_use":
			id, _ := block.MustGet("id").(string)
			name, _ := block.MustGet("name").(string)
			builder.Tool(id, name, block.MustGet("input"))
		}
	}
}
