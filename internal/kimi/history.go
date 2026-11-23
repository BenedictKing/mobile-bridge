package kimi

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

type TranscriptSource struct {
	index      *wireIndex
	mu         sync.Mutex
	signatures map[string][2]int64
	pending    map[string]bool
}

// TranscriptReader loads the main agent's wire.jsonl without starting Kimi.
func TranscriptReader(home string) func(string) (*acpview.NativeSnapshot, error) {
	return NewTranscriptReader(home).Load
}

// NewTranscriptReader shares change detection between live and history reads.
func NewTranscriptReader(home string) *TranscriptSource {
	return &TranscriptSource{index: &wireIndex{root: filepath.Join(home, "sessions")},
		signatures: map[string][2]int64{}, pending: map[string]bool{}}
}

// Reset forces the next read to rebuild when a new view or handle opens.
func (r *TranscriptSource) Reset(sessionID string) {
	r.mu.Lock()
	delete(r.signatures, sessionID)
	delete(r.pending, sessionID)
	r.mu.Unlock()
}

func (r *TranscriptSource) Load(sessionID string) (*acpview.NativeSnapshot, error) {
	return r.load(sessionID, false)
}

// LoadFresh bypasses change detection for a one-off state check immediately
// before an automatic continuation is submitted.
func (r *TranscriptSource) LoadFresh(sessionID string) (*acpview.NativeSnapshot, error) {
	return r.load(sessionID, true)
}

func (r *TranscriptSource) load(sessionID string, force bool) (*acpview.NativeSnapshot, error) {
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
	defer r.mu.Unlock()
	previous, seen := r.signatures[sessionID]
	pending := r.pending[sessionID]
	stale := time.Since(info.ModTime()) >= historyStaleAfter
	if seen && previous == signature && !force {
		if !pending || !stale {
			return nil, nil
		}
	}
	snapshot, err := readHistory(path, stale)
	if err != nil || snapshot == nil {
		return nil, err
	}
	r.signatures[sessionID] = signature
	r.pending[sessionID] = nativeHistoryActive(snapshot)
	return snapshot, nil
}

func readHistory(path string, stale bool) (*acpview.NativeSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer file.Close()
	sessionDir := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	sessionID := filepath.Base(sessionDir)
	builder := acpview.NewNativeBuilder(sessionID)
	status := "completed"
	var origins []string
	seenUsers := make(map[string]bool)
	if row, _ := readSessionRow(filepath.Dir(sessionDir), sessionID); row != nil {
		if title, _ := row.MustGet("title").(string); title != "" {
			builder.SetTitle(title)
		}
	}
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
		titleText := ""
		switch kind {
		case "turn.prompt":
			if historyInternalOrigin(event.MustGet("origin")) {
				if builder.HasTurns() {
					status = "inProgress"
				}
				continue
			}
			titleText = acpview.Text(event.MustGet("input"))
			id, _ := event.MustGet("promptId").(string)
			if id != "" && seenUsers[id] {
				continue
			}
			builder.User(titleText)
			if titleText != "" {
				if id != "" {
					seenUsers[id] = true
				}
				origins = append(origins, historyOrigin(event.MustGet("origin")))
				status = "inProgress"
			}
		case "context.append_message":
			message, _ := event.MustGet("message").(*jsoncompat.Object)
			if message != nil && message.MustGet("role") == "user" {
				if historyInternalOrigin(message.MustGet("origin")) || acpview.Text(message.MustGet("content")) == "" {
					continue
				}
				id, _ := message.MustGet("id").(string)
				if id != "" && seenUsers[id] {
					// Kimi persists turn.prompt again as a user-message copy.
					continue
				}
				if id != "" {
					seenUsers[id] = true
				}
				origins = append(origins, historyOrigin(message.MustGet("origin")))
				status = "inProgress"
			}
			applyKimiMessage(builder, message)
		case "context.append_loop_event":
			applyKimiLoopEvent(builder, event.MustGet("event"))
		case "turn.ended":
			reason, _ := event.MustGet("reason").(string)
			builder.End(reason)
			status = "completed"
			switch reason {
			case "cancelled":
				status = "interrupted"
			case "failed", "blocked":
				status = "failed"
			}
		case "turn.cancel":
			builder.End("cancelled")
			status = "interrupted"
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
	snapshot := builder.Snapshot(stale)
	if snapshot != nil {
		if status == "inProgress" && stale {
			status = "interrupted"
			snapshot.Turns[len(snapshot.Turns)-1].Set("stopReason", "cancelled")
		}
		snapshot.Turns[len(snapshot.Turns)-1].Set("status", status)
		if status == "inProgress" {
			snapshot.Turns[len(snapshot.Turns)-1].Delete("stopReason")
		}
		if len(origins) == len(snapshot.Turns) {
			snapshot.TurnOrigins = origins
		}
	}
	return snapshot, nil
}

func historyOrigin(value any) string {
	if origin, ok := value.(string); ok {
		return origin
	}
	if origin, ok := value.(*jsoncompat.Object); ok {
		kind, _ := origin.MustGet("kind").(string)
		return kind
	}
	return ""
}

func historyInternalOrigin(value any) bool {
	if historyOrigin(value) == "skill_activation" {
		return true
	}
	if origin, ok := value.(string); ok {
		return internalTitleOrigins[origin]
	}
	return internalTitleOrigin(value)
}

func applyKimiMessage(builder *acpview.NativeBuilder, value any) {
	message, _ := value.(*jsoncompat.Object)
	if message == nil {
		return
	}
	role, _ := message.MustGet("role").(string)
	content := message.MustGet("content")
	if role == "tool" {
		id, _ := message.MustGet("toolCallId").(string)
		failed, _ := message.MustGet("isError").(bool)
		builder.ToolResult(id, content, failed)
		return
	}
	if role == "user" {
		if historyInternalOrigin(message.MustGet("origin")) {
			return
		}
		builder.User(acpview.Text(content))
		return
	}
	if role != "assistant" {
		return
	}
	blocks, _ := content.(*jsoncompat.Array)
	if blocks == nil {
		if text, ok := content.(string); ok {
			builder.Agent(text)
		}
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
		case "think":
			text, _ := block.MustGet("think").(string)
			builder.Thought(text)
		}
	}
	calls, _ := message.MustGet("toolCalls").(*jsoncompat.Array)
	if calls == nil {
		return
	}
	for _, item := range calls.Items {
		call, _ := item.(*jsoncompat.Object)
		if call == nil {
			continue
		}
		id, _ := call.MustGet("id").(string)
		name, _ := call.MustGet("name").(string)
		builder.Tool(id, name, call.MustGet("args"))
	}
}

func applyKimiLoopEvent(builder *acpview.NativeBuilder, value any) {
	event, _ := value.(*jsoncompat.Object)
	if event == nil {
		return
	}
	switch event.MustGet("type") {
	case "content.part":
		part, _ := event.MustGet("part").(*jsoncompat.Object)
		if part == nil {
			return
		}
		switch part.MustGet("type") {
		case "text":
			text, _ := part.MustGet("text").(string)
			builder.Agent(text)
		case "think":
			text, _ := part.MustGet("think").(string)
			builder.Thought(text)
		}
	case "tool.call":
		id, _ := event.MustGet("toolCallId").(string)
		name, _ := event.MustGet("name").(string)
		builder.Tool(id, name, event.MustGet("args"))
	case "tool.result":
		id, _ := event.MustGet("toolCallId").(string)
		failed, _ := event.MustGet("isError").(bool)
		builder.ToolResult(id, event.MustGet("result"), failed)
	}
}

func nativeHistoryActive(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}
