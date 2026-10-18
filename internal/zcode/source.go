// Package zcode reads ZCode's own session database.
//
// The gateway reaches ZCode through zcode-acp, which streams session/update
// notifications only for prompts that wrapper process runs itself. A turn
// started in the ZCode desktop app against the same session is persisted to
// ~/.zcode/cli/db/db.sqlite but reaches the gateway only as session/load
// history — the phone used to show "修复" and then nothing while the desktop
// was still working. Source turns that SQLite state into an acpview snapshot
// so the live view can follow desktop-driven turns.
package zcode

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// staleTurnAfter bounds how long an in-progress turn may go without a store
// write before the view calls it interrupted (desktop app quit/crashed).
const staleTurnAfter = 15 * time.Minute

// Source is a read-only handle on one ZCode session database. It is safe for
// concurrent Load calls; max one connection is enough for the sub-ms polls.
type Source struct {
	db *sql.DB

	mu         sync.Mutex
	signatures map[string]string
	pending    map[string]bool
	now        func() time.Time
	staleAfter time.Duration
	// artifactDir is <zcode-home>/cli/artifacts; mediaDir receives decoded
	// artifact images for the phone's desktop-image route.
	artifactDir string
	mediaDir    string
}

// OpenSource opens path read-only. A missing database is an error for the
// caller to log once; Load itself stays silent.
func OpenSource(path string) (*Source, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("zcode session store %s is a directory", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Source{db: db, signatures: map[string]string{}, pending: map[string]bool{},
		now: time.Now, staleAfter: staleTurnAfter,
		artifactDir: filepath.Join(filepath.Dir(filepath.Dir(path)), "artifacts")}, nil
}

// SetMediaDir enables decoding zcode-artifact image attachments into files the
// phone can preview. Empty keeps only attachments whose source path exists.
func (s *Source) SetMediaDir(dir string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.mediaDir = dir
	s.mu.Unlock()
}

// Reset forgets the cached change signature for sessionID so the next Load
// rebuilds even when the store has not moved — a freshly attached handle
// needs the current conversation, not just changes since the last handle.
func (s *Source) Reset(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.signatures, sessionID)
	delete(s.pending, sessionID)
	s.mu.Unlock()
}

// Close releases the read-only connection.
func (s *Source) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Load returns the session's persisted conversation. A nil snapshot means the
// store has not moved since the previous call, so pollers skip the rebuild.
func (s *Source) Load(sessionID string) (*acpview.NativeSnapshot, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	signature, updated, found, err := s.signature(sessionID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	stale := false
	if previous, ok := s.signatures[sessionID]; ok && previous == signature {
		if !s.pending[sessionID] {
			return nil, nil
		}
		if s.now().Sub(time.UnixMilli(updated)) < s.staleAfter {
			return nil, nil
		}
		stale = true
	}
	snapshot, err := s.snapshot(sessionID, stale)
	if err != nil {
		return nil, err
	}
	s.signatures[sessionID] = signature
	s.pending[sessionID] = lastTurnInProgress(snapshot)
	return snapshot, nil
}

// signature covers every column a rendered turn depends on: message ordering
// and completion, part text/tool state, turn settlements, the session title
// and the todo plan.
func (s *Source) signature(sessionID string) (string, int64, bool, error) {
	var title string
	var updated, lastMessage, lastMessageUpdate, lastPart, lastTurn, lastTodo int64
	err := s.db.QueryRow(`
		SELECT s.title, s.time_updated,
			COALESCE((SELECT MAX(sequence) FROM message WHERE session_id = s.id), 0),
			COALESCE((SELECT MAX(time_updated) FROM message WHERE session_id = s.id), 0),
			COALESCE((SELECT MAX(time_updated) FROM part WHERE session_id = s.id), 0),
			COALESCE((SELECT MAX(completed_at) FROM turn_usage WHERE session_id = s.id), 0),
			COALESCE((SELECT MAX(time_updated) FROM todo WHERE session_id = s.id), 0)
		FROM session s WHERE s.id = ?`, sessionID).
		Scan(&title, &updated, &lastMessage, &lastMessageUpdate, &lastPart, &lastTurn, &lastTodo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d", title, updated, lastMessage,
		lastMessageUpdate, lastPart, lastTurn, lastTodo), updated, true, nil
}

type storedMessage struct {
	id       string
	sequence int64
	created  int64
	data     *jsoncompat.Object
}

type storedPart struct {
	id        string
	messageID string
	sequence  int64
	data      *jsoncompat.Object
}

type turnUsage struct {
	status    string
	cancelled int64
	completed int64
	errorType string
}

type storedTodo struct {
	content string
	status  string
}

// snapshot rebuilds the whole visible conversation from the store. Message
// and part ids are kept stable so timeline row keys survive each poll.
func (s *Source) snapshot(sessionID string, stale bool) (*acpview.NativeSnapshot, error) {
	var title string
	var created, updated int64
	if err := s.db.QueryRow(`SELECT title, time_created, time_updated FROM session WHERE id = ?`,
		sessionID).Scan(&title, &created, &updated); err != nil {
		return nil, err
	}

	messages, err := s.messages(sessionID)
	if err != nil {
		return nil, err
	}
	parts, err := s.parts(sessionID)
	if err != nil {
		return nil, err
	}
	usage, err := s.usages(sessionID)
	if err != nil {
		return nil, err
	}
	todos, err := s.todos(sessionID)
	if err != nil {
		return nil, err
	}
	return s.buildSnapshot(sessionID, title, messages, parts, usage, todos, stale), nil
}

func (s *Source) messages(sessionID string) ([]storedMessage, error) {
	rows, err := s.db.Query(`SELECT id, sequence, time_created, data FROM message
		WHERE session_id = ? ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []storedMessage{}
	for rows.Next() {
		var message storedMessage
		var raw []byte
		if err := rows.Scan(&message.id, &message.sequence, &message.created, &raw); err != nil {
			return nil, err
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			continue
		}
		message.data, _ = value.(*jsoncompat.Object)
		if message.data != nil {
			messages = append(messages, message)
		}
	}
	return messages, rows.Err()
}

func (s *Source) parts(sessionID string) (map[string][]storedPart, error) {
	rows, err := s.db.Query(`SELECT id, message_id, sequence, data FROM part
		WHERE session_id = ? ORDER BY message_id, sequence`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := map[string][]storedPart{}
	for rows.Next() {
		var part storedPart
		var raw []byte
		if err := rows.Scan(&part.id, &part.messageID, &part.sequence, &raw); err != nil {
			return nil, err
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			continue
		}
		part.data, _ = value.(*jsoncompat.Object)
		if part.data == nil {
			continue
		}
		parts[part.messageID] = append(parts[part.messageID], part)
	}
	return parts, rows.Err()
}

func (s *Source) usages(sessionID string) (map[string]turnUsage, error) {
	rows, err := s.db.Query(`SELECT turn_id, status, cancelled_by_user, completed_at,
		error_type FROM turn_usage WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usages := map[string]turnUsage{}
	for rows.Next() {
		var turnID string
		var usage turnUsage
		var errorType sql.NullString
		if err := rows.Scan(&turnID, &usage.status, &usage.cancelled,
			&usage.completed, &errorType); err != nil {
			return nil, err
		}
		usage.errorType = errorType.String
		usages[turnID] = usage
	}
	return usages, rows.Err()
}

func (s *Source) todos(sessionID string) ([]storedTodo, error) {
	rows, err := s.db.Query(`SELECT content, status FROM todo WHERE session_id = ?
		ORDER BY position`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	todos := []storedTodo{}
	for rows.Next() {
		var todo storedTodo
		if err := rows.Scan(&todo.content, &todo.status); err != nil {
			return nil, err
		}
		todos = append(todos, todo)
	}
	return todos, rows.Err()
}

// nativeTurn accumulates one turn while messages are walked in sequence order.
type nativeTurn struct {
	id         string
	startedAt  int64
	messages   *jsoncompat.Array
	lastDone   int64 // completion time of the last visible message
	lastRole   string
	lastFinish string // finish reason of the last assistant message
}

func (s *Source) buildSnapshot(sessionID, title string, messages []storedMessage,
	parts map[string][]storedPart, usages map[string]turnUsage, todos []storedTodo,
	stale bool) *acpview.NativeSnapshot {
	turns := []*nativeTurn{}
	for _, message := range messages {
		if !visibleMessage(message.data) {
			continue
		}
		role, _ := message.data.MustGet("role").(string)
		if role != "user" && role != "assistant" {
			continue
		}
		turnID := turnIDOf(message.data, message.sequence)
		turn := currentTurn(turns, turnID)
		if turn == nil {
			turn = &nativeTurn{id: turnID, startedAt: message.created, messages: &jsoncompat.Array{}}
			turns = append(turns, turn)
		}
		if role == "user" {
			if text := userText(parts[message.id]); text != "" {
				turn.messages.Items = append(turn.messages.Items, userRow(message.id, text, s.userAttachments(sessionID, parts[message.id])))
			}
			turn.lastDone = 0
			turn.lastRole = "user"
			continue
		}
		turn.lastDone = messageCompletion(message.data)
		turn.lastFinish, _ = message.data.MustGet("finish").(string)
		turn.lastRole = "assistant"
		for _, part := range parts[message.id] {
			if row := partRow(part); row != nil {
				turn.messages.Items = append(turn.messages.Items, row)
			}
		}
	}
	// A visible assistant message waits for its next completion marker; an
	// in-progress turn is the only one allowed to end at the tip.
	snapshot := &acpview.NativeSnapshot{Title: title, Turns: []*jsoncompat.Object{}}
	for index, turn := range turns {
		status := turnStatus(turn, usages[turn.id], index == len(turns)-1)
		snapshot.Turns = append(snapshot.Turns, turnObject(turn, status))
	}
	if stale && len(snapshot.Turns) > 0 {
		if last := snapshot.Turns[len(snapshot.Turns)-1]; turnStatusOf(last) == "inProgress" {
			last.Set("status", "interrupted")
		}
	}
	if len(snapshot.Turns) > 0 {
		if plan := planRow(todos); plan != nil {
			last, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("messages").(*jsoncompat.Array)
			if last != nil {
				last.Items = append(last.Items, plan)
			}
		}
	}
	return snapshot
}

func turnStatusOf(turn *jsoncompat.Object) string {
	if turn == nil {
		return ""
	}
	status, _ := turn.MustGet("status").(string)
	return status
}

func lastTurnInProgress(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	return turnStatusOf(snapshot.Turns[len(snapshot.Turns)-1]) == "inProgress"
}

func currentTurn(turns []*nativeTurn, turnID string) *nativeTurn {
	if len(turns) == 0 {
		return nil
	}
	if last := turns[len(turns)-1]; last.id == turnID {
		return last
	}
	return nil
}

func turnIDOf(message *jsoncompat.Object, sequence int64) string {
	if anchor, ok := message.MustGet("anchor").(*jsoncompat.Object); ok && anchor != nil {
		if turnID, _ := anchor.MustGet("turnId").(string); turnID != "" {
			return turnID
		}
	}
	return fmt.Sprintf("turn-%d", sequence)
}

func messageCompletion(message *jsoncompat.Object) int64 {
	if value, ok := message.MustGet("time").(*jsoncompat.Object); ok && value != nil {
		if completed, ok := value.MustGet("completed").(json.Number); ok {
			result, _ := completed.Int64()
			return result
		}
	}
	return 0
}

func turnStatus(turn *nativeTurn, usage turnUsage, last bool) string {
	if usage.cancelled != 0 {
		return "interrupted"
	}
	switch usage.status {
	case "completed":
		return "completed"
	case "cancelled", "canceled", "interrupted":
		return "interrupted"
	case "failed", "error":
		return "failed"
	}
	if usage.errorType != "" {
		return "failed"
	}
	if usage.completed != 0 {
		return "completed"
	}
	if !last {
		return "completed"
	}
	if turn.lastRole == "user" || turn.lastDone == 0 {
		return "inProgress"
	}
	// "tool-calls" means the model asked for tools and the turn continues
	// with the next step; "stop"/"completed" ends it.
	if turn.lastFinish == "tool-calls" {
		return "inProgress"
	}
	return "completed"
}

func turnObject(turn *nativeTurn, status string) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", turn.id)
	object.Set("status", status)
	object.Set("startedAt", jsoncompat.Num(int(turn.startedAt)))
	object.Set("messages", turn.messages)
	object.Set("actionable", false)
	object.Set("error", nil)
	object.Set("diff", nil)
	return object
}

// visibleMessage drops prompts and replies the desktop UI hides: todo
// reminders, compaction summaries and other model-only input.
func visibleMessage(message *jsoncompat.Object) bool {
	if message == nil {
		return false
	}
	if synthetic, _ := message.MustGet("synthetic").(bool); synthetic {
		return false
	}
	if semantics, ok := message.MustGet("semantics").(*jsoncompat.Object); ok && semantics != nil {
		if visibility, _ := semantics.MustGet("uiVisibility").(string); visibility == "hidden" {
			return false
		}
	}
	if metadata, ok := message.MustGet("metadata").(*jsoncompat.Object); ok && metadata != nil {
		if visibility, _ := metadata.MustGet("visibility").(string); visibility == "model-only" {
			return false
		}
		if source, _ := metadata.MustGet("source").(string); source == "todo_reminder" {
			return false
		}
	}
	return true
}

// userText reads a user message's text parts. ZCode keeps message bodies in
// the part table, not in message.data.
func userText(parts []storedPart) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.data == nil {
			continue
		}
		if kind, _ := part.data.MustGet("type").(string); kind == "text" {
			if text, _ := part.data.MustGet("text").(string); text != "" {
				builder.WriteString(text)
			}
		}
	}
	return builder.String()
}

func userRow(id, text string, attachments *jsoncompat.Array) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("id", "zcode-"+id)
	row.Set("kind", "userMessage")
	row.Set("role", "user")
	row.Set("text", text)
	if attachments != nil && len(attachments.Items) > 0 {
		row.Set("attachments", attachments)
	}
	return row
}

// userAttachments turns the user message's file parts into phone attachments.
// Existing local files keep their path (served by /desktop-images); images
// whose temp file is gone are decoded from the zcode-artifact store.
func (s *Source) userAttachments(sessionID string, parts []storedPart) *jsoncompat.Array {
	list := &jsoncompat.Array{}
	for _, part := range parts {
		if part.data == nil {
			continue
		}
		if kind, _ := part.data.MustGet("type").(string); kind != "file" {
			continue
		}
		name, _ := part.data.MustGet("filename").(string)
		mime, _ := part.data.MustGet("mime").(string)
		path := ""
		if strings.HasPrefix(mime, "image/") {
			sourcePath := ""
			if source, _ := part.data.MustGet("source").(*jsoncompat.Object); source != nil {
				if candidate, _ := source.MustGet("path").(string); candidate != "" && regularFile(candidate) {
					sourcePath = candidate
					path = s.cacheImage(sessionID, candidate)
				}
			}
			if path == "" {
				if url, _ := part.data.MustGet("url").(string); url != "" {
					path = s.artifactImage(sessionID, url)
				}
			}
			// The app's temp screenshots can be unreadable under TCC; the
			// artifact copy above is the durable path. Last resort keeps the
			// original so a permission-enabled gateway can still serve it.
			if path == "" {
				path = sourcePath
			}
		}
		if path == "" {
			if name != "" {
				row := jsoncompat.NewObject()
				row.Set("type", "file")
				row.Set("name", name)
				list.Items = append(list.Items, row)
			}
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("type", "localImage")
		row.Set("path", path)
		if name != "" {
			row.Set("name", name)
		}
		list.Items = append(list.Items, row)
	}
	return list
}

// artifactImage resolves a zcode-artifact URL to its persisted image. The
// artifact store keeps a data URL in a .txt file named after the URL token;
// the decoded bytes are cached under mediaDir so /desktop-images can serve
// them without touching zcode's store on every poll.
func (s *Source) artifactImage(sessionID, rawURL string) string {
	rest := strings.TrimPrefix(rawURL, "zcode-artifact://")
	if rest == rawURL {
		return ""
	}
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return ""
	}
	artifactSession, token := rest[:slash], rest[slash+1:]
	if artifactSession != sessionID || token == "" || strings.ContainsAny(token, `/\`) {
		return ""
	}
	// Load holds s.mu for the whole build; read the fields directly.
	artifactDir, mediaDir := s.artifactDir, s.mediaDir
	if mediaDir == "" || !safeName(sessionID) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(artifactDir, sessionID, "*"+token+"*"))
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || info.IsDir() || info.Size() > 32<<20 {
			continue
		}
		raw, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		decoded := decodeDataURL(strings.TrimSpace(string(raw)))
		if len(decoded) == 0 {
			continue
		}
		sum := sha256.Sum256(decoded)
		dir := filepath.Join(mediaDir, "zcode-native", sessionID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return ""
		}
		path := filepath.Join(dir, hex.EncodeToString(sum[:])+".bin")
		if _, err := os.Stat(path); err != nil {
			if err := os.WriteFile(path, decoded, 0o600); err != nil {
				return ""
			}
		}
		return path
	}
	return ""
}

// cacheImage copies an existing temp screenshot into the gateway's media
// directory once, so the attachment keeps working after macOS cleans
// /var/folders. Empty mediaDir keeps the original path.
func (s *Source) cacheImage(sessionID, path string) string {
	mediaDir := s.mediaDir
	if mediaDir == "" {
		return path
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > 32<<20 || !safeName(sessionID) {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	dir := filepath.Join(mediaDir, "zcode-native", sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	extension := filepath.Ext(path)
	if len(extension) > 8 {
		extension = ""
	}
	target := filepath.Join(dir, hex.EncodeToString(sum[:])+extension)
	if _, err := os.Stat(target); err == nil {
		return target
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return ""
	}
	return target
}

func decodeDataURL(text string) []byte {
	if !strings.HasPrefix(text, "data:") {
		return nil
	}
	comma := strings.IndexByte(text, ',')
	if comma < 0 {
		return nil
	}
	header := text[:comma]
	if !strings.Contains(header, ";base64") {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(text[comma+1:])
	if err != nil {
		return nil
	}
	return decoded
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func safeName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func partRow(part storedPart) *jsoncompat.Object {
	data := part.data
	if data == nil {
		return nil
	}
	kind, _ := data.MustGet("type").(string)
	switch kind {
	case "reasoning":
		text, _ := data.MustGet("text").(string)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		row := jsoncompat.NewObject()
		row.Set("id", "zcode-"+part.id)
		row.Set("kind", "agentThought")
		row.Set("role", "activity")
		row.Set("title", "思考摘要")
		row.Set("text", text)
		return row
	case "text":
		text, _ := data.MustGet("text").(string)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		row := jsoncompat.NewObject()
		row.Set("id", "zcode-"+part.id)
		row.Set("kind", "agentMessage")
		row.Set("role", "assistant")
		row.Set("phase", "final")
		row.Set("text", text)
		return row
	case "tool":
		return toolRow(part.id, data)
	}
	return nil
}

func toolRow(id string, data *jsoncompat.Object) *jsoncompat.Object {
	state, _ := data.MustGet("state").(*jsoncompat.Object)
	if state == nil {
		return nil
	}
	title, _ := state.MustGet("title").(string)
	if title == "" {
		title, _ = data.MustGet("tool").(string)
	}
	if title == "" {
		title = "工具调用"
	}
	row := jsoncompat.NewObject()
	row.Set("id", "zcode-"+id)
	row.Set("kind", "toolCall")
	row.Set("role", "activity")
	row.Set("title", title)
	row.Set("text", acpview.CommandSummary(state.MustGet("input")))
	row.Set("output", toolOutput(state.MustGet("output")))
	row.Set("status", toolState(state))
	return row
}

func toolOutput(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		return jsoncompat.PyStr(typed)
	}
}

// toolState folds ZCode's tool lifecycle onto the ACP names the view and the
// phone already understand.
func toolState(state *jsoncompat.Object) string {
	status, _ := state.MustGet("status").(string)
	switch status {
	case "completed":
		return "completed"
	case "failed", "error":
		return "failed"
	case "running", "pending", "in_progress":
		return "inProgress"
	}
	if state.MustGet("output") != nil {
		return "completed"
	}
	return "inProgress"
}

func planRow(todos []storedTodo) *jsoncompat.Object {
	if len(todos) == 0 {
		return nil
	}
	entries := &jsoncompat.Array{}
	lines := []string{}
	allCompleted := true
	for _, todo := range todos {
		entry := jsoncompat.NewObject()
		entry.Set("content", todo.content)
		entry.Set("status", todo.status)
		entries.Items = append(entries.Items, entry)
		mark, ok := map[string]string{"completed": "✓", "in_progress": "▸", "pending": "○"}[todo.status]
		if !ok {
			mark = "○"
		}
		lines = append(lines, mark+" "+todo.content)
		if todo.status != "completed" {
			allCompleted = false
		}
	}
	row := jsoncompat.NewObject()
	row.Set("id", "zcode-plan")
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
