package claude

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/agentmedia"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
)

const (
	historyMaxBytes     = 256 << 20
	historyMaxLineBytes = 16 << 20
	historyStaleAfter   = 6 * time.Minute
)

type historyReader struct {
	index      *transcriptIndex
	mu         sync.Mutex
	signatures map[string][2]int64
	pending    map[string]bool
	mediaDir   string
}

// TranscriptReader loads Claude's persisted main-chain events without starting
// claude-agent-acp. A nil snapshot means the file is unchanged or has no turns.
func TranscriptReader(root string, mediaDirs ...string) func(string) (*acpview.NativeSnapshot, error) {
	reader := &historyReader{index: &transcriptIndex{root: root},
		signatures: map[string][2]int64{}, pending: map[string]bool{}}
	if len(mediaDirs) > 0 {
		reader.mediaDir = mediaDirs[0]
	}
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
	snapshot, _, err := readHistory(path, stale, r.mediaDir)
	if err != nil || snapshot == nil {
		return nil, err
	}
	r.mu.Lock()
	r.signatures[sessionID] = signature
	r.pending[sessionID] = nativeHistoryActive(snapshot) || backgroundHistoryPending(snapshot)
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
		snapshot, cwd, err := readHistory(path, time.Since(info.ModTime()) >= historyStaleAfter, "")
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

func readHistory(path string, stale bool, mediaDir string) (*acpview.NativeSnapshot, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", nil
	}
	defer file.Close()
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	builder := acpview.NewNativeBuilder(sessionID)
	backgroundTasks := map[string]string{}
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
		agentmedia.PersistImages(content, mediaDir, sessionID)
		titleText := ""
		switch kind {
		case "user":
			origin, _ := event.MustGet("origin").(*jsoncompat.Object)
			originKind := ""
			if origin != nil {
				originKind, _ = origin.MustGet("kind").(string)
			}
			if originKind == "task-notification" {
				if applyBackgroundTaskNotification(builder, content, backgroundTasks) {
					continue
				}
			}
			titleText = applyClaudeUser(builder, content, backgroundTasks)
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
	if stale {
		for _, toolID := range backgroundTasks {
			builder.BackgroundTask(toolID, "outcome_unknown", "后台任务长时间没有新记录，结果待确认；未自动重试。")
		}
	}
	return builder.Snapshot(stale), cwd, nil
}

func backgroundHistoryPending(snapshot *acpview.NativeSnapshot) bool {
	for _, turn := range snapshot.Turns {
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, value := range messages.Items {
			message, _ := value.(*jsoncompat.Object)
			if message != nil && message.MustGet("backgroundTask") == true && message.MustGet("status") == "inProgress" {
				return true
			}
		}
	}
	return false
}

func nativeHistoryActive(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}

func applyClaudeUser(builder *acpview.NativeBuilder, content any, backgroundTasks map[string]string) string {
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
		result := block.MustGet("content")
		if taskID := backgroundTaskID(result); taskID != "" && id != "" {
			backgroundTasks[taskID] = id
			builder.ToolResult(id, result, failed)
			builder.BackgroundTask(id, "in_progress", "命令已转入后台，Claude 将在完成后继续。")
		} else {
			builder.ToolResult(id, result, failed)
		}
	}
	if hasToolResult {
		return ""
	}
	text := acpview.Text(blocks)
	attachments := acpview.ContentAttachments(blocks)
	if text == "" && len(attachments) == 0 {
		for _, item := range blocks.Items {
			if block, _ := item.(*jsoncompat.Object); block != nil && block.MustGet("type") == "image" {
				text = "[图片]"
				break
			}
		}
	}
	return foldUserText(builder, text, attachments...)
}

var backgroundTaskIDPattern = regexp.MustCompile(`(?m)^Command running in background with ID: ([^\s.]+)`)
var backgroundExitCodePattern = regexp.MustCompile(`\bexit code (-?[0-9]+)\b`)

func backgroundTaskID(content any) string {
	match := backgroundTaskIDPattern.FindStringSubmatch(acpview.Text(content))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func applyBackgroundTaskNotification(builder *acpview.NativeBuilder, content any, backgroundTasks map[string]string) bool {
	text := acpview.Text(content)
	if !strings.Contains(text, "<task-notification>") {
		return false
	}
	taskID := notificationTag(text, "task-id")
	toolUseID := notificationTag(text, "tool-use-id")
	if toolUseID == "" {
		toolUseID = backgroundTasks[taskID]
	}
	if toolUseID == "" {
		return false
	}
	delete(backgroundTasks, taskID)
	status := "outcome_unknown"
	summary := notificationTag(text, "summary")
	switch notificationTag(text, "status") {
	case "completed":
		status = "completed"
		if match := backgroundExitCodePattern.FindStringSubmatch(summary); len(match) == 2 && match[1] != "0" {
			status = "failed"
		}
	case "failed":
		status = "failed"
	}
	if summary == "" {
		summary = "后台任务已结束，等待 Claude 继续处理。"
	} else {
		summary = "后台任务结果：" + summary
	}
	builder.BackgroundTask(toolUseID, status, summary)
	return true
}

func notificationTag(text, name string) string {
	open, close := "<"+name+">", "</"+name+">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(text[start:], close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

// foldUserText handles one user-side text. Claude Code echoes a user
// interrupt as a literal user record ("[Request interrupted by user]" or its
// tool-use variant) with no turn-end after it; folding it as a normal prompt
// leaves a forever-inProgress turn that keeps the phone read-only. The
// interrupted turn settles as interrupted and the marker itself renders as a
// closed turn instead.
func foldUserText(builder *acpview.NativeBuilder, text string, attachments ...*jsoncompat.Object) string {
	if strings.HasPrefix(text, "[Request interrupted by user") {
		builder.End("cancelled")
		builder.User(text)
		builder.End("cancelled")
		return ""
	}
	builder.User(text, attachments...)
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
		case "image":
			builder.AgentContent(block)
		case "tool_use", "server_tool_use", "mcp_tool_use":
			id, _ := block.MustGet("id").(string)
			name, _ := block.MustGet("name").(string)
			builder.Tool(id, name, block.MustGet("input"))
		}
	}
}
