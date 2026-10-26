package dsh

// The DSH app persists every session as an append-only zstd JSONL transcript
// (session.v4.jsonl.zstd). dsh-acp refuses to load a session the desktop app
// currently owns ("already owned by an active write handle"), so the phone
// falls back to this transcript for a read-only view: it carries the same
// user/assistant/tool events the app renders. Reading it never touches the
// app or the session's write lock.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	// A session transcript is normally a few megabytes; the caps keep a
	// pathological file from stalling the 1.5s poller.
	maxTranscriptBytes = 64 << 20
	maxDecodedBytes    = 256 << 20
	maxEvents          = 200000
	maxToolOutput      = 200 << 10
	maxLineBytes       = 16 << 20
	// A turn with no turn/end and no transcript writes for this long is
	// treated as interrupted: the desktop app quit or crashed mid-turn and
	// the phone must not stay on "working" forever.
	staleTurnAfter = 15 * time.Minute
)

var errTranscriptTooLarge = errors.New("dsh transcript too large for the native reader")

// Reader turns the newest transcript of one DSH session into an acpview
// snapshot. Load returns nil when the file has not moved since the previous
// call, so the poller can skip rebuilding.
type Reader struct {
	root string

	mu         sync.Mutex
	signatures map[string]fileSignature
	pending    map[string]bool
	now        func() time.Time
	staleAfter time.Duration
}

type fileSignature struct {
	size    int64
	updated int64
}

// TranscriptReader returns the loader the acpruntime manager polls for
// desktop-owned DSH sessions.
func TranscriptReader(root string) func(sessionID string) (*acpview.NativeSnapshot, error) {
	return newReader(root).Load
}

// NewTranscriptReader exposes the reader so the gateway can share one change
// cache between the live follower and the read-only fallback, and reset it
// when a fresh ACP handle attaches.
func NewTranscriptReader(root string) *Reader {
	return newReader(root)
}

// Reset forgets the cached signature so the next Load rebuilds after a new
// handle attaches.
func (r *Reader) Reset(sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.signatures, sessionID)
	delete(r.pending, sessionID)
	r.mu.Unlock()
}

func newReader(root string) *Reader {
	return &Reader{root: root, signatures: map[string]fileSignature{}, pending: map[string]bool{},
		now: time.Now, staleAfter: staleTurnAfter}
}

// Load parses the session's transcript when its file signature changed.
func (r *Reader) Load(sessionID string) (*acpview.NativeSnapshot, error) {
	if r == nil || sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return nil, nil
	}
	path, signature, ok := r.transcript(sessionID)
	if !ok {
		return nil, nil
	}
	r.mu.Lock()
	previous, seen := r.signatures[sessionID]
	pending := r.pending[sessionID]
	r.mu.Unlock()
	stale := false
	if seen && previous == signature {
		if !pending {
			return nil, nil
		}
		if r.now().Sub(time.UnixMilli(signature.updated)) < r.staleAfter {
			return nil, nil
		}
		stale = true
	}
	snapshot, err := r.read(sessionID, path, stale)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.signatures[sessionID] = signature
	r.pending[sessionID] = lastTurnInProgress(snapshot)
	r.mu.Unlock()
	return snapshot, nil
}

func (r *Reader) transcript(sessionID string) (string, fileSignature, bool) {
	matches, err := filepath.Glob(filepath.Join(r.root, "sessions", "*", sessionID, "session.*"))
	if err != nil {
		return "", fileSignature{}, false
	}
	newest := ""
	var signature fileSignature
	for _, match := range matches {
		if strings.HasSuffix(match, ".lock") {
			continue
		}
		info, err := os.Stat(match)
		if err != nil || info.IsDir() {
			continue
		}
		if updated := info.ModTime().UnixMilli(); updated >= signature.updated {
			newest, signature = match, fileSignature{size: info.Size(), updated: updated}
		}
	}
	return newest, signature, newest != ""
}

func (r *Reader) read(sessionID, path string, stale bool) (*acpview.NativeSnapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTranscriptBytes {
		return nil, errTranscriptTooLarge
	}
	decoded := raw
	if strings.HasSuffix(path, ".zstd") {
		decoder, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer decoder.Close()
		var decodeErr error
		decoded, decodeErr = io.ReadAll(io.LimitReader(decoder, maxDecodedBytes+1))
		if len(decoded) > maxDecodedBytes {
			return nil, errTranscriptTooLarge
		}
		// The app appends frames while we read: a truncated tail still leaves
		// every complete earlier frame usable.
		if decodeErr != nil && len(decoded) == 0 {
			return nil, decodeErr
		}
	}
	return parseTranscript(sessionID, r.root, decoded, stale), nil
}

type transcriptTurn struct {
	id        string
	startedAt int64
	status    string
	messages  *jsoncompat.Array
}

type transcriptParser struct {
	root       string
	title      string
	model      string
	effort     string
	turns      []*transcriptTurn
	current    *transcriptTurn
	tools      map[string]*jsoncompat.Object
	todos      []*jsoncompat.Object
	lastTurnNo int
}

func parseTranscript(sessionID, root string, raw []byte, stale bool) *acpview.NativeSnapshot {
	parser := &transcriptParser{root: root, tools: map[string]*jsoncompat.Object{}}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	events := 0
	for scanner.Scan() {
		events++
		if events > maxEvents {
			break
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		value, err := jsoncompat.Unmarshal(line)
		if err != nil {
			continue
		}
		event, _ := value.(*jsoncompat.Object)
		if event != nil {
			parser.apply(event)
		}
	}
	if len(parser.turns) == 0 {
		return nil
	}
	if stale {
		if last := parser.turns[len(parser.turns)-1]; last.status == "inProgress" {
			last.status = "interrupted"
		}
	}
	// Only visible turns reach the phone.
	turns := make([]*jsoncompat.Object, 0, len(parser.turns))
	for _, turn := range parser.turns {
		if turn.messages == nil || len(turn.messages.Items) == 0 {
			continue
		}
		status := turn.status
		if status == "" {
			status = "completed"
		}
		turns = append(turns, nativeTurn(turn, status))
	}
	if len(turns) == 0 {
		return nil
	}
	if parser.todos != nil {
		if last, _ := turns[len(turns)-1].MustGet("messages").(*jsoncompat.Array); last != nil {
			last.Items = append(last.Items, planRow(parser.todos))
		}
	}
	return &acpview.NativeSnapshot{Title: parser.title, Model: parser.model, Effort: parser.effort, Turns: turns}
}

func (p *transcriptParser) apply(event *jsoncompat.Object) {
	kind, _ := event.MustGet("type").(string)
	seq := number(event.MustGet("seq"))
	at := number(event.MustGet("time"))
	data, _ := event.MustGet("data").(*jsoncompat.Object)
	switch kind {
	case "session/title":
		if title, _ := data.MustGet("title").(string); strings.TrimSpace(title) != "" {
			p.title = strings.TrimSpace(title)
		}
	case "turn/start":
		p.startTurn(number(data.MustGet("turn")), at)
	case "user/message":
		p.userMessage(seq, at, data)
	case "assistant/message":
		p.assistantMessage(seq, data)
	case "tool/result":
		p.toolResult(data)
	case "turn/end":
		if p.current != nil {
			p.current.status = turnReason(data.MustGet("reason"))
			p.current = nil
		}
	case "todo/write":
		p.todos = todoEntries(data.MustGet("todos"))
	case "request/header":
		p.requestHeader(data)
	}
}

// requestHeader keeps the model the app is actually talking to; the native
// view has no ACP session to ask for configOptions.
func (p *transcriptParser) requestHeader(data *jsoncompat.Object) {
	header, _ := data.MustGet("header").(*jsoncompat.Object)
	if header == nil {
		return
	}
	config, _ := header.MustGet("config").(*jsoncompat.Object)
	if config == nil {
		return
	}
	provider, _ := config.MustGet("provider").(string)
	model, _ := config.MustGet("model").(string)
	if model != "" {
		if provider != "" {
			p.model = provider + "::" + model
		} else {
			p.model = model
		}
	}
	if effort, _ := config.MustGet("reasoningEffort").(string); effort != "" {
		p.effort = effort
	}
}

func (p *transcriptParser) startTurn(turn, at int64) *transcriptTurn {
	if turn <= 0 {
		turn = int64(len(p.turns) + 1)
	}
	state := &transcriptTurn{
		id:        "turn-" + numberText(turn),
		startedAt: at,
		status:    "inProgress",
		messages:  &jsoncompat.Array{},
	}
	p.turns = append(p.turns, state)
	p.current = state
	p.lastTurnNo = int(turn)
	return state
}

func (p *transcriptParser) currentTurn() *transcriptTurn {
	if p.current != nil {
		return p.current
	}
	return p.startTurn(int64(p.lastTurnNo+1), 0)
}

func (p *transcriptParser) userMessage(seq, at int64, data *jsoncompat.Object) {
	if data == nil {
		return
	}
	if source, _ := data.MustGet("source").(*jsoncompat.Object); source != nil {
		if kind, _ := source.MustGet("kind").(string); kind != "user" {
			return
		}
	}
	turn := p.currentTurn()
	if turn.startedAt == 0 {
		turn.startedAt = at
	}
	content, _ := data.MustGet("content").(*jsoncompat.Array)
	text := ""
	attachments := &jsoncompat.Array{}
	for _, item := range arrItems(content) {
		block, _ := item.(*jsoncompat.Object)
		if block == nil {
			continue
		}
		switch blockType(block) {
		case "text":
			if part, _ := block.MustGet("text").(string); part != "" {
				text += part
			}
		case "image":
			if attachment := p.imageAttachment(block); attachment != nil {
				attachments.Items = append(attachments.Items, attachment)
			}
		}
	}
	if strings.TrimSpace(text) == "" && len(attachments.Items) == 0 {
		return
	}
	id, _ := data.MustGet("id").(string)
	if id == "" {
		id = "seq-" + numberText(seq)
	}
	row := jsoncompat.NewObject()
	row.Set("id", "dsh-user-"+id)
	row.Set("kind", "userMessage")
	row.Set("role", "user")
	row.Set("text", text)
	if len(attachments.Items) > 0 {
		row.Set("attachments", attachments)
	}
	turn.messages.Items = append(turn.messages.Items, row)
}

func (p *transcriptParser) assistantMessage(seq int64, data *jsoncompat.Object) {
	if data == nil {
		return
	}
	message, _ := data.MustGet("message").(*jsoncompat.Object)
	if message == nil {
		return
	}
	content, _ := message.MustGet("content").(*jsoncompat.Array)
	turn := p.currentTurn()
	for index, item := range arrItems(content) {
		block, _ := item.(*jsoncompat.Object)
		if block == nil {
			continue
		}
		switch blockType(block) {
		case "reasoning":
			text, _ := block.MustGet("text").(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			row := jsoncompat.NewObject()
			row.Set("id", "dsh-"+numberText(seq)+"-"+numberText(int64(index)))
			row.Set("kind", "agentThought")
			row.Set("role", "activity")
			row.Set("title", "思考摘要")
			row.Set("text", text)
			turn.messages.Items = append(turn.messages.Items, row)
		case "text":
			text, _ := block.MustGet("text").(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			row := jsoncompat.NewObject()
			row.Set("id", "dsh-"+numberText(seq)+"-"+numberText(int64(index)))
			row.Set("kind", "agentMessage")
			row.Set("role", "assistant")
			row.Set("phase", "final")
			row.Set("text", text)
			turn.messages.Items = append(turn.messages.Items, row)
		case "tool-call":
			if row := p.toolCall(block); row != nil {
				turn.messages.Items = append(turn.messages.Items, row)
			}
		}
	}
}

func (p *transcriptParser) toolCall(block *jsoncompat.Object) *jsoncompat.Object {
	callID, _ := block.MustGet("id").(string)
	if callID == "" {
		return nil
	}
	name, _ := block.MustGet("name").(string)
	if name == "" {
		name = "工具调用"
	}
	row := jsoncompat.NewObject()
	row.Set("id", "dsh-tool-"+callID)
	row.Set("kind", "toolCall")
	row.Set("role", "activity")
	row.Set("title", name)
	row.Set("text", acpview.CommandSummary(parseArguments(block.MustGet("arguments"))))
	row.Set("status", "inProgress")
	p.tools[callID] = row
	return row
}

func (p *transcriptParser) toolResult(data *jsoncompat.Object) {
	if data == nil {
		return
	}
	message, _ := data.MustGet("message").(*jsoncompat.Object)
	if message == nil {
		return
	}
	callID, _ := message.MustGet("toolCallId").(string)
	if callID == "" {
		callID, _ = data.MustGet("toolCallId").(string)
	}
	row := p.tools[callID]
	if row == nil {
		if callID == "" {
			return
		}
		row = jsoncompat.NewObject()
		row.Set("id", "dsh-tool-"+callID)
		row.Set("kind", "toolCall")
		row.Set("role", "activity")
		row.Set("title", "工具调用")
		row.Set("text", "")
		p.tools[callID] = row
		p.currentTurn().messages.Items = append(p.currentTurn().messages.Items, row)
	}
	if output := contentText(message.MustGet("content")); output != "" {
		row.Set("output", truncate(output, maxToolOutput))
	}
	if failed, _ := message.MustGet("isError").(bool); failed {
		row.Set("status", "failed")
	} else {
		row.Set("status", "completed")
	}
	if attachments := imageAttachments(p, message.MustGet("content")); len(attachments.Items) > 0 {
		row.Set("attachments", attachments)
	}
}

func (p *transcriptParser) imageAttachment(block *jsoncompat.Object) *jsoncompat.Object {
	attachment, _ := block.MustGet("attachment").(*jsoncompat.Object)
	if attachment == nil {
		return nil
	}
	id, _ := attachment.MustGet("attachmentId").(string)
	name, _ := attachment.MustGet("name").(string)
	path := attachmentPath(p.root, id)
	if path == "" {
		return nil
	}
	row := jsoncompat.NewObject()
	row.Set("type", "localImage")
	row.Set("path", path)
	if name != "" {
		row.Set("name", name)
	}
	return row
}

func isHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// attachmentPath resolves DSH's content-addressed attachment store:
// <home>/attachments/v1/objects/<ff>/<hash>.
func attachmentPath(root, attachmentID string) string {
	hash := strings.TrimPrefix(attachmentID, "sha256:")
	if len(hash) != 64 || !isHex(hash) {
		return ""
	}
	path := filepath.Join(root, "attachments", "v1", "objects", hash[:2], hash)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

func lastTurnInProgress(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}

func nativeTurn(turn *transcriptTurn, status string) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("id", turn.id)
	row.Set("status", status)
	row.Set("startedAt", jsoncompat.Num(int(turn.startedAt)))
	row.Set("messages", turn.messages)
	row.Set("actionable", false)
	row.Set("error", nil)
	row.Set("diff", nil)
	return row
}

func turnReason(value any) string {
	reason, _ := value.(*jsoncompat.Object)
	kind := ""
	if reason != nil {
		kind, _ = reason.MustGet("kind").(string)
	}
	switch kind {
	case "cancelled", "canceled", "interrupted", "aborted":
		return "interrupted"
	case "failed", "error":
		return "failed"
	default:
		return "completed"
	}
}

func todoEntries(value any) []*jsoncompat.Object {
	list, _ := value.(*jsoncompat.Array)
	rows := []*jsoncompat.Object{}
	for _, item := range arrItems(list) {
		todo, _ := item.(*jsoncompat.Object)
		if todo == nil {
			continue
		}
		content, _ := todo.MustGet("content").(string)
		status, _ := todo.MustGet("status").(string)
		if strings.TrimSpace(content) == "" {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("content", content)
		row.Set("status", status)
		rows = append(rows, row)
	}
	return rows
}

func planRow(todos []*jsoncompat.Object) *jsoncompat.Object {
	entries := &jsoncompat.Array{}
	lines := []string{}
	allCompleted := true
	for _, todo := range todos {
		entries.Items = append(entries.Items, todo)
		status, _ := todo.MustGet("status").(string)
		content, _ := todo.MustGet("content").(string)
		mark := map[string]string{"completed": "✓", "in_progress": "▸", "pending": "○"}[status]
		if mark == "" {
			mark = "○"
		}
		lines = append(lines, mark+" "+content)
		if status != "completed" {
			allCompleted = false
		}
	}
	row := jsoncompat.NewObject()
	row.Set("id", "dsh-plan")
	row.Set("kind", "plan")
	row.Set("role", "activity")
	row.Set("title", "计划")
	row.Set("text", strings.Join(lines, "\n"))
	row.Set("entries", entries)
	if allCompleted {
		row.Set("status", "completed")
	} else {
		row.Set("status", nil)
	}
	return row
}

func parseArguments(value any) any {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return value
	}
	parsed, err := jsoncompat.Unmarshal([]byte(text))
	if err != nil {
		return text
	}
	return parsed
}

func imageAttachments(p *transcriptParser, content any) *jsoncompat.Array {
	list, _ := content.(*jsoncompat.Array)
	attachments := &jsoncompat.Array{}
	for _, item := range arrItems(list) {
		block, _ := item.(*jsoncompat.Object)
		if block == nil || blockType(block) != "image" {
			continue
		}
		if attachment := p.imageAttachment(block); attachment != nil {
			attachments.Items = append(attachments.Items, attachment)
		}
	}
	return attachments
}

func contentText(value any) string {
	list, _ := value.(*jsoncompat.Array)
	if list == nil {
		if text, ok := value.(string); ok {
			return text
		}
		return ""
	}
	builder := strings.Builder{}
	for _, item := range arrItems(list) {
		block, _ := item.(*jsoncompat.Object)
		if block == nil {
			continue
		}
		if blockType(block) == "text" {
			if text, _ := block.MustGet("text").(string); text != "" {
				builder.WriteString(text)
			}
		}
	}
	return builder.String()
}

func blockType(block *jsoncompat.Object) string {
	value, _ := block.MustGet("type").(string)
	return value
}

func arrItems(list *jsoncompat.Array) []any {
	if list == nil {
		return nil
	}
	return list.Items
}

func number(value any) int64 {
	parsed, _ := value.(json.Number)
	result, _ := parsed.Int64()
	return result
}

func numberText(value int64) string {
	return jsoncompat.Num(int(value)).String()
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
