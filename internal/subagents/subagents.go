// Package subagents ports bridge/agents/native_subagents.py: on-disk
// sub-agent discovery for ACP agents that archive children as their own
// sessions instead of embedding them in the parent transcript.
//
// Each store speaks one agent's native layout and projects both a child
// roster and a readable history in the message vocabulary the activity
// flatten renders. Stores are read-only and fail soft: missing or unreadable
// files simply yield no children, because a foreign on-disk layout must
// never break the live ACP session it decorates.
//
// dsh archives sub-sessions as zstd-compressed event logs and the gateway
// has no zstd support, so it stays undiscoverable here (same as Python).
package subagents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// One child transcript is bounded the same way as any other projection
// source: real transcripts reach tens of megabytes, but the card only needs
// the story.
const (
	maxTranscriptBytes = 8 * 1024 * 1024
	maxEvents          = 4000
	maxChildren        = 32
	cacheLimit         = 128
)

// Child is one roster row; Transcript rides along for history lookups.
type Child struct {
	ID               string
	Name             string
	AgentRole        string
	SpawnStatus      string
	UpdatedAtMs      int64
	FirstUserMessage string
	Transcript       string
}

// Row is one flattened parent activity row (Python's _subagent_activity).
type Row struct {
	ID             string
	Kind           string
	Role           string
	Title          string
	Text           string
	SubagentID     string
	SubagentType   string
	SubagentPrompt string
	StopReason     string
	Status         string
}

// Activity is the injection payload: flattened rows plus the roster revision
// (max updated_at_ms, 0 when there are no children).
type Activity struct {
	Rows     []Row
	Revision int64
}

// Store discovers children for one agent's on-disk layout.
type Store interface {
	Children(parentID string) []Child
	History(childID string) *jsoncompat.Object
}

func readJSON(path string) *jsoncompat.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return nil
	}
	object, _ := value.(*jsoncompat.Object)
	return object
}

// iterLines streams JSONL with a hard byte budget; partial or bad lines are
// dropped. The callback returns false to stop early.
func iterLines(path string, byteCap int64, consume func(*jsoncompat.Object) bool) {
	handle, err := os.Open(path)
	if err != nil {
		return
	}
	defer handle.Close()
	reader := json.NewDecoder(handle)
	var consumed int64
	for {
		if consumed > byteCap {
			return
		}
		var raw json.RawMessage
		if err := reader.Decode(&raw); err != nil {
			return
		}
		consumed += int64(len(raw))
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			continue
		}
		object, _ := value.(*jsoncompat.Object)
		if object == nil {
			continue
		}
		if !consume(object) {
			return
		}
	}
}

// text flattens message content blocks the way text_content does.
func text(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case *jsoncompat.Array:
		parts := []string{}
		for _, item := range typed.Items {
			block, ok := item.(*jsoncompat.Object)
			if !ok {
				continue
			}
			if kind, _ := block.MustGet("type").(string); kind == "text" || kind == "input_text" || kind == "output_text" {
				if value, _ := block.MustGet("text").(string); value != "" {
					parts = append(parts, value)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func pyJSON(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return string(jsoncompat.EncodePythonDefaultASCII(value))
}

// isoMs mirrors Python _iso_ms: numeric values pass through as int, ISO
// strings parse to epoch milliseconds, everything else is 0.
func isoMs(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
		if parsed, err := typed.Float64(); err == nil {
			return int64(parsed)
		}
	case string:
		for _, layout := range []string{"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05Z07:00", time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, typed); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

// historyBuilder shared transcript folding: the three stores emit per-event
// callbacks onto a shared accumulator that mirrors Python's turn/item loops.
type historyBuilder struct {
	turns   []*jsoncompat.Object
	items   []*jsoncompat.Object
	pending map[string]*jsoncompat.Object
}

func newHistoryBuilder() *historyBuilder {
	return &historyBuilder{pending: map[string]*jsoncompat.Object{}}
}

func (b *historyBuilder) endTurn(status string) {
	turn := jsoncompat.NewObject()
	turn.Set("turnId", jsoncompat.Num(len(b.turns)))
	turn.Set("status", status)
	turn.Set("messages", &jsoncompat.Array{Items: toAny(b.items)})
	b.turns = append(b.turns, turn)
	b.items = nil
}

func (b *historyBuilder) userMessage(content string) {
	if content == "" {
		return
	}
	message := jsoncompat.NewObject()
	message.Set("id", "user")
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", truncate(content, 16000))
	b.items = append(b.items, message)
}

func (b *historyBuilder) agentMessage(content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	message := jsoncompat.NewObject()
	message.Set("id", "agent")
	message.Set("kind", "agentMessage")
	message.Set("role", "assistant")
	message.Set("text", truncate(content, 16000))
	b.items = append(b.items, message)
}

func (b *historyBuilder) toolItem(callID, name, arguments any) *jsoncompat.Object {
	id := pyStr(callID)
	message := jsoncompat.NewObject()
	message.Set("id", id)
	message.Set("kind", "toolCall")
	message.Set("role", "activity")
	title := pyStr(name)
	if title == "" {
		title = "工具调用"
	}
	message.Set("title", title)
	message.Set("text", truncate(pyJSON(arguments), 0))
	b.pending[id] = message
	b.items = append(b.items, message)
	return message
}

func (b *historyBuilder) finish(id, title string) *jsoncompat.Object {
	if len(b.items) > 0 {
		b.endTurn("completed")
	}
	result := jsoncompat.NewObject()
	result.Set("id", id)
	result.Set("title", title)
	result.Set("turns", &jsoncompat.Array{Items: toAny(b.turns)})
	return result
}

func pyStr(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return jsoncompat.PyStr(value)
}

func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func toAny(items []*jsoncompat.Object) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

// baseStore mirrors _BaseStore: signature caches for roster and history.
type baseStore struct {
	root string
	mu   sync.Mutex
	rosterCache map[string]cacheEntry
	historyCache map[string]cacheEntry
}

type cacheEntry struct {
	signature [2]int64
	value     any
}

func (s *baseStore) cached(cache map[string]cacheEntry, key string, signature [2]int64, build func() any) any {
	s.mu.Lock()
	if hit, ok := cache[key]; ok && hit.signature == signature {
		s.mu.Unlock()
		return hit.value
	}
	s.mu.Unlock()
	value := build()
	s.mu.Lock()
	if len(cache) > cacheLimit {
		for name := range cache {
			delete(cache, name)
		}
	}
	cache[key] = cacheEntry{signature: signature, value: value}
	s.mu.Unlock()
	return value
}

func signatureOf(path string) [2]int64 {
	if info, err := os.Stat(path); err == nil {
		return [2]int64{info.ModTime().UnixNano(), info.Size()}
	}
	return [2]int64{}
}

// allRosterRows: history lookups arrive right after a roster scan; transcript
// paths ride along on the roster rows.
func (s *baseStore) allRosterRows() []Child {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := []Child{}
	for _, cached := range s.rosterCache {
		children, _ := cached.value.([]Child)
		rows = append(rows, children...)
	}
	return rows
}

// ZcodeStore: ~/.zcode/v2/acp-lazy-sessions.json maps the ACP session id to
// the backend sess_* id; ~/.zcode/cli/agents/<sid>/agent_*/ holds one
// directory per spawned child with metadata.json and transcript.jsonl.
type ZcodeStore struct{ baseStore }

func NewZcodeStore(root string) *ZcodeStore {
	return &ZcodeStore{baseStore{root: root, rosterCache: map[string]cacheEntry{}, historyCache: map[string]cacheEntry{}}}
}

func (s *ZcodeStore) zcodeSid(acpSessionID string) string {
	mapping := readJSON(filepath.Join(s.root, "v2", "acp-lazy-sessions.json"))
	if mapping != nil {
		if entry, ok := mapping.MustGet(acpSessionID).(*jsoncompat.Object); ok {
			if sid, ok := entry.MustGet("zcodeSid").(string); ok && sid != "" {
				return sid
			}
		}
	}
	// Sessions created before the lazy map existed already use backend ids.
	if strings.HasPrefix(acpSessionID, "sess_") {
		return acpSessionID
	}
	return ""
}

func (s *ZcodeStore) Children(parentID string) []Child {
	sid := s.zcodeSid(parentID)
	if sid == "" {
		return nil
	}
	agentsDir := filepath.Join(s.root, "cli", "agents", sid)
	build := func() any {
		matches, _ := filepath.Glob(filepath.Join(agentsDir, "agent_*", "metadata.json"))
		sort.Strings(matches)
		rows := []Child{}
		for _, metaPath := range matches {
			if len(rows) >= maxChildren {
				break
			}
			meta := readJSON(metaPath)
			if meta == nil {
				continue
			}
			childID, _ := meta.MustGet("childSessionId").(string)
			if childID == "" {
				continue
			}
			name, _ := meta.MustGet("description").(string)
			if name == "" {
				name, _ = meta.MustGet("prompt").(string)
			}
			if name == "" {
				name = childID
			}
			role, _ := meta.MustGet("profileId").(string)
			status, _ := meta.MustGet("status").(string)
			updated := isoMs(meta.MustGet("updatedAt"))
			if updated == 0 {
				updated = isoMs(meta.MustGet("completedAt"))
			}
			prompt, _ := meta.MustGet("prompt").(string)
			rows = append(rows, Child{ID: childID, Name: name, AgentRole: role, SpawnStatus: status,
				UpdatedAtMs: updated, FirstUserMessage: truncate(prompt, 4000),
				Transcript: filepath.Join(filepath.Dir(metaPath), "transcript.jsonl")})
		}
		return rows
	}
	children, _ := s.cached(s.rosterCache, sid, signatureOf(agentsDir), build).([]Child)
	return children
}

func (s *ZcodeStore) History(childID string) *jsoncompat.Object {
	var transcript, name, id string
	for _, candidate := range s.allRosterRows() {
		if candidate.ID == childID {
			transcript, name, id = candidate.Transcript, candidate.Name, candidate.ID
			break
		}
	}
	if transcript == "" {
		return jsoncompat.NewObject()
	}
	path := transcript
	build := func() any {
		builder := newHistoryBuilder()
		events := 0
		iterLines(path, maxTranscriptBytes, func(event *jsoncompat.Object) bool {
			events++
			if events > maxEvents {
				return false
			}
			kind, _ := event.MustGet("type").(string)
			payload, _ := event.MustGet("payload").(*jsoncompat.Object)
			if payload == nil {
				payload = jsoncompat.NewObject()
			}
			switch kind {
			case "turn_started":
				if len(builder.items) > 0 {
					builder.endTurn("completed")
				}
				prompt := payload.MustGet("input")
				if promptText, ok := prompt.(string); ok {
					builder.userMessage(promptText)
				} else {
					builder.userMessage(text(prompt))
				}
			case "tool_call_scheduled":
				builder.toolItem(payload.MustGet("toolCallId"), payload.MustGet("toolName"), payload.MustGet("input"))
			case "model_complete":
				if content, ok := payload.MustGet("content").(string); ok {
					builder.agentMessage(content)
				}
			case "turn_complete":
				resultType, _ := payload.MustGet("resultType").(string)
				failed := resultType != "" && resultType != "success" && resultType != "completed"
				response := strings.TrimSpace(pyStr(payload.MustGet("response")))
				builder.agentMessage(response)
				status := "completed"
				if failed {
					status = "failed"
				}
				builder.endTurn(status)
			}
			return true
		})
		return builder.finish(id, name)
	}
	history, _ := s.cached(s.historyCache, id, signatureOf(path), build).(*jsoncompat.Object)
	return history
}

// KimiStore: ~/.kimi-code/session_index.jsonl locates the session directory;
// state.json lists spawned agents and agents/<id>/wire.jsonl records each
// child's execution as native loop events.
type KimiStore struct{ baseStore }

func NewKimiStore(root string) *KimiStore {
	return &KimiStore{baseStore{root: root, rosterCache: map[string]cacheEntry{}, historyCache: map[string]cacheEntry{}}}
}

func (s *KimiStore) sessionDir(sessionID string) string {
	found := ""
	iterLines(filepath.Join(s.root, "session_index.jsonl"), 1024*1024, func(entry *jsoncompat.Object) bool {
		id, _ := entry.MustGet("sessionId").(string)
		dir, _ := entry.MustGet("sessionDir").(string)
		if id == sessionID && dir != "" {
			found = dir
			return false
		}
		return true
	})
	return found
}

func (s *KimiStore) Children(parentID string) []Child {
	sessionDir := s.sessionDir(parentID)
	if sessionDir == "" {
		return nil
	}
	statePath := filepath.Join(sessionDir, "state.json")
	build := func() any {
		state := readJSON(statePath)
		if state == nil {
			return []Child{}
		}
		spawnTypes, statuses := s.spawnTypes(sessionDir)
		agents, _ := state.MustGet("agents").(*jsoncompat.Object)
		rows := []Child{}
		if agents == nil {
			return rows
		}
		for _, agentID := range agents.Keys() {
			agent, _ := agents.MustGet(agentID).(*jsoncompat.Object)
			if agent == nil {
				continue
			}
			if kind, _ := agent.MustGet("type").(string); kind != "sub" {
				continue
			}
			// 'agent-0' repeats in every session; the parent prefix keeps
			// child ids unique across the roster cache.
			status := statuses[agentID]
			if status == "" {
				status = "completed"
			}
			lastPrompt, _ := state.MustGet("lastPrompt").(string)
			rows = append(rows, Child{ID: parentID + "/" + agentID, Name: agentID,
				AgentRole: spawnTypes[agentID], SpawnStatus: status,
				UpdatedAtMs: isoMs(state.MustGet("updatedAt")),
				FirstUserMessage: truncate(lastPrompt, 4000),
				Transcript:      filepath.Join(sessionDir, "agents", agentID, "wire.jsonl")})
			if len(rows) >= maxChildren {
				break
			}
		}
		return rows
	}
	children, _ := s.cached(s.rosterCache, parentID, signatureOf(statePath), build).([]Child)
	return children
}

// spawnTypes: child profile names ride on task.started events
// (info.kind=agent) in the main agent's wire; task.terminated closes them out.
func (s *KimiStore) spawnTypes(sessionDir string) (map[string]string, map[string]string) {
	types := map[string]string{}
	statuses := map[string]string{}
	iterLines(filepath.Join(sessionDir, "agents", "main", "wire.jsonl"), 4*1024*1024, func(event *jsoncompat.Object) bool {
		info, _ := event.MustGet("info").(*jsoncompat.Object)
		if info == nil {
			return true
		}
		if kind, _ := info.MustGet("kind").(string); kind != "agent" {
			return true
		}
		agentID, _ := info.MustGet("agentId").(string)
		if agentID == "" {
			return true
		}
		if subagentType, _ := info.MustGet("subagentType").(string); subagentType != "" {
			if _, exists := types[agentID]; !exists {
				types[agentID] = subagentType
			}
		}
		if kind, _ := event.MustGet("type").(string); kind == "task.terminated" {
			if status, _ := info.MustGet("status").(string); status != "" {
				statuses[agentID] = status
			}
		}
		return true
	})
	return types, statuses
}

func (s *KimiStore) History(childID string) *jsoncompat.Object {
	var row *Child
	for _, candidate := range s.allRosterRows() {
		if candidate.ID == childID {
			row = &candidate
			break
		}
	}
	if row == nil {
		return jsoncompat.NewObject()
	}
	path := row.Transcript
	build := func() any {
		builder := newHistoryBuilder()
		iterLines(path, maxTranscriptBytes, func(event *jsoncompat.Object) bool {
			kind, _ := event.MustGet("type").(string)
			switch kind {
			case "turn.prompt":
				if len(builder.items) > 0 {
					builder.endTurn("completed")
				}
				builder.userMessage(text(event.MustGet("input")))
			case "context.append_message":
				message, _ := event.MustGet("message").(*jsoncompat.Object)
				if message == nil {
					return true
				}
				content := text(message.MustGet("content"))
				if content == "" {
					return true
				}
				if role, _ := message.MustGet("role").(string); role == "user" {
					builder.userMessage(content)
				} else {
					builder.agentMessage(content)
				}
			case "context.append_loop_event":
				loop, _ := event.MustGet("event").(*jsoncompat.Object)
				if loop == nil {
					return true
				}
				loopKind, _ := loop.MustGet("type").(string)
				if loopKind == "tool.call" {
					builder.toolItem(loop.MustGet("toolCallId"), loop.MustGet("name"), loop.MustGet("args"))
				} else if loopKind == "tool.result" {
					id := pyStr(loop.MustGet("toolCallId"))
					if item := builder.pending[id]; item != nil {
						item.Set("output", truncate(pyJSON(loop.MustGet("result")), 16000))
					}
				}
			case "turn.ended":
				reason, _ := event.MustGet("reason").(string)
				status := "completed"
				if reason != "" && reason != "completed" && reason != "stop" {
					status = "failed"
				}
				builder.endTurn(status)
			}
			return true
		})
		return builder.finish(childID, row.Name)
	}
	// Child ids like 'agent-0' repeat across sessions; the transcript path
	// is the real identity.
	history, _ := s.cached(s.historyCache, path, signatureOf(path), build).(*jsoncompat.Object)
	return history
}

// ClaudeStore: ~/.claude/projects/<slug>/<sessionId>/subagents/agent-*.jsonl;
// the directory name is the parent session id, so discovery is one glob.
type ClaudeStore struct{ baseStore }

func NewClaudeStore(root string) *ClaudeStore {
	return &ClaudeStore{baseStore{root: root, rosterCache: map[string]cacheEntry{}, historyCache: map[string]cacheEntry{}}}
}

func (s *ClaudeStore) subagentsDir(parentID string) string {
	if strings.TrimSpace(parentID) == "" {
		return ""
	}
	projects, _ := filepath.Glob(filepath.Join(s.root, "projects", "*"))
	for _, directory := range projects {
		candidate := filepath.Join(directory, parentID, "subagents")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

func (s *ClaudeStore) Children(parentID string) []Child {
	directory := s.subagentsDir(parentID)
	if directory == "" {
		return nil
	}
	build := func() any {
		matches, _ := filepath.Glob(filepath.Join(directory, "agent-*.jsonl"))
		sort.Strings(matches)
		rows := []Child{}
		for index, path := range matches {
			if index >= maxChildren {
				break
			}
			var first *jsoncompat.Object
			lastStamp := any(nil)
			iterLines(path, 256*1024, func(event *jsoncompat.Object) bool {
				if first == nil {
					first = event
				}
				if stamp := event.MustGet("timestamp"); stamp != nil {
					lastStamp = stamp
				}
				return true
			})
			if first == nil {
				continue
			}
			agentID, _ := first.MustGet("agentId").(string)
			if agentID == "" {
				agentID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
			}
			role := agentID
			if strings.HasPrefix(role, "agent-") {
				trimmed := role[len("agent-"):]
				if idx := strings.LastIndex(trimmed, "-"); idx >= 0 {
					trimmed = trimmed[:idx]
				}
				if trimmed != "" {
					role = trimmed
				}
			}
			var preview string
			if message, ok := first.MustGet("message").(*jsoncompat.Object); ok {
				preview = truncate(text(message.MustGet("content")), 4000)
			}
			stamp := lastStamp
			if stamp == nil {
				stamp = first.MustGet("timestamp")
			}
			rows = append(rows, Child{ID: path, Name: agentID, AgentRole: role, SpawnStatus: "completed",
				UpdatedAtMs: isoMs(stamp), FirstUserMessage: preview, Transcript: path})
		}
		return rows
	}
	children, _ := s.cached(s.rosterCache, parentID, signatureOf(directory), build).([]Child)
	return children
}

func (s *ClaudeStore) History(childID string) *jsoncompat.Object {
	path := childID
	build := func() any {
		builder := newHistoryBuilder()
		iterLines(path, maxTranscriptBytes, func(event *jsoncompat.Object) bool {
			kind, _ := event.MustGet("type").(string)
			message, _ := event.MustGet("message").(*jsoncompat.Object)
			content := any(nil)
			if message != nil {
				content = message.MustGet("content")
			}
			isMeta := jsoncompat.Truthy(event.MustGet("isMeta"))
			switch kind {
			case "user":
				if textContent, ok := content.(string); ok {
					if strings.TrimSpace(textContent) != "" && !isMeta {
						if len(builder.items) > 0 {
							builder.endTurn("completed")
						}
						builder.userMessage(textContent)
					}
				} else if blocks, ok := content.(*jsoncompat.Array); ok {
					for _, item := range blocks.Items {
						block, ok := item.(*jsoncompat.Object)
						if !ok {
							continue
						}
						blockType, _ := block.MustGet("type").(string)
						if blockType == "text" {
							if value, _ := block.MustGet("text").(string); strings.TrimSpace(value) != "" && !isMeta {
								builder.userMessage(value)
							}
						} else if blockType == "tool_result" {
							if item := builder.pending[pyStr(block.MustGet("tool_use_id"))]; item != nil {
								item.Set("output", truncate(text(block.MustGet("content")), 16000))
							}
						}
					}
				}
			case "assistant":
				if blocks, ok := content.(*jsoncompat.Array); ok {
					for _, item := range blocks.Items {
						block, ok := item.(*jsoncompat.Object)
						if !ok {
							continue
						}
						blockType, _ := block.MustGet("type").(string)
						if blockType == "text" {
							if value, _ := block.MustGet("text").(string); strings.TrimSpace(value) != "" {
								builder.agentMessage(value)
							}
						} else if blockType == "tool_use" {
							builder.toolItem(block.MustGet("id"), block.MustGet("name"), block.MustGet("input"))
						}
					}
				}
			}
			return true
		})
		return builder.finish(strings.TrimSuffix(filepath.Base(path), ".jsonl"), strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	}
	history, _ := s.cached(s.historyCache, childID, signatureOf(path), build).(*jsoncompat.Object)
	return history
}

// Flatten ports _subagent_activity: fold child-agent rollouts into
// expandable parent activity rows plus the roster revision.
func Flatten(store Store, parentID string) Activity {
	children := store.Children(parentID)
	activity := Activity{Rows: []Row{}}
	for _, child := range children {
		activity.Revision = max64(activity.Revision, child.UpdatedAtMs)
		if child.ID == "" {
			continue
		}
		history := store.History(child.ID)
		lines := []string{}
		if turns, ok := history.MustGet("turns").(*jsoncompat.Array); ok {
			for _, turnValue := range turns.Items {
				turn, ok := turnValue.(*jsoncompat.Object)
				if !ok {
					continue
				}
				messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
				if messages == nil {
					continue
				}
				for _, messageValue := range messages.Items {
					message, ok := messageValue.(*jsoncompat.Object)
					if !ok {
						continue
					}
					textContent := strings.TrimSpace(pyStr(message.MustGet("text")))
					output := strings.TrimSpace(pyStr(message.MustGet("output")))
					if textContent == "" && output == "" {
						continue
					}
					role, _ := message.MustGet("role").(string)
					title, _ := message.MustGet("title").(string)
					if title == "" {
						switch role {
						case "user":
							title = "用户"
						case "assistant":
							title = "Agent"
						default:
							if kind, _ := message.MustGet("kind").(string); kind != "" {
								title = kind
							} else {
								title = "activity"
							}
						}
					}
					content := textContent
					if output != "" {
						if content != "" {
							content += "\n\n"
						}
						content += output
					}
					lines = append(lines, "["+title+"]\n"+content)
				}
			}
		}
		description := child.Name
		if description == "" {
			description = child.AgentRole
		}
		if description == "" {
			description = "子 Agent"
		}
		subagentType := child.AgentRole
		title := "子 Agent"
		if subagentType != "" {
			title += " · " + subagentType
		}
		title += " · " + description
		status := child.SpawnStatus
		if status == "" {
			status = "completed"
		}
		rowText := strings.Join(lines, "\n\n")
		if rowText == "" {
			rowText = "子 Agent 暂无可显示的执行记录"
		}
		activity.Rows = append(activity.Rows, Row{
			ID: "subagent:" + child.ID, Kind: "subagent", Role: "activity",
			Title: title, Text: rowText, SubagentID: child.ID, SubagentType: subagentType,
			SubagentPrompt: child.FirstUserMessage, Status: status,
		})
	}
	return activity
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Inject ports _add_subagents: the revision lands unconditionally (0 without
// children), the flattened rows join the last turn's messages, deduped by id.
func Inject(view *jsoncompat.Object, activity Activity) *jsoncompat.Object {
	view.Set("subagentRevision", jsoncompat.Num(int(activity.Revision)))
	if len(activity.Rows) == 0 {
		return view
	}
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		turns = &jsoncompat.Array{}
		view.Set("turns", turns)
	}
	if len(turns.Items) == 0 {
		turn := jsoncompat.NewObject()
		turn.Set("id", "subagents")
		turn.Set("status", "completed")
		turn.Set("messages", &jsoncompat.Array{})
		turns.Items = append(turns.Items, turn)
	}
	last, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	if last == nil {
		return view
	}
	messages, _ := last.MustGet("messages").(*jsoncompat.Array)
	if messages == nil {
		messages = &jsoncompat.Array{}
		last.Set("messages", messages)
	}
	known := map[string]bool{}
	for _, turnValue := range turns.Items {
		if turn, ok := turnValue.(*jsoncompat.Object); ok {
			if list, ok := turn.MustGet("messages").(*jsoncompat.Array); ok {
				for _, messageValue := range list.Items {
					if message, ok := messageValue.(*jsoncompat.Object); ok {
						if id, _ := message.MustGet("id").(string); id != "" {
							known[id] = true
						}
					}
				}
			}
		}
	}
	for _, row := range activity.Rows {
		if known[row.ID] {
			continue
		}
		message := jsoncompat.NewObject()
		message.Set("id", row.ID)
		message.Set("kind", row.Kind)
		message.Set("role", row.Role)
		message.Set("title", row.Title)
		message.Set("text", row.Text)
		message.Set("subagentId", row.SubagentID)
		message.Set("subagentType", row.SubagentType)
		message.Set("subagentPrompt", row.SubagentPrompt)
		message.Set("stopReason", row.StopReason)
		message.Set("status", row.Status)
		messages.Items = append(messages.Items, message)
	}
	return view
}


// Flattener caches the flatten output per parent keyed by the roster
// revision, so per-view decoration (SSE polls read the view repeatedly)
// only stats the roster instead of re-walking child transcripts.
type Flattener struct {
	store Store
	mu    sync.Mutex
	cache map[string]cachedFlatten
}

type cachedFlatten struct {
	revision int64
	activity Activity
}

// NewFlattener wraps one store with revision-keyed activity caching.
func NewFlattener(store Store) *Flattener {
	return &Flattener{store: store, cache: map[string]cachedFlatten{}}
}

// Activity returns the flattened rows for one parent session.
func (f *Flattener) Activity(parentID string) Activity {
	revision := int64(0)
	for _, child := range f.store.Children(parentID) {
		revision = max64(revision, child.UpdatedAtMs)
	}
	f.mu.Lock()
	if hit, ok := f.cache[parentID]; ok && hit.revision == revision {
		f.mu.Unlock()
		return hit.activity
	}
	f.mu.Unlock()
	activity := Flatten(f.store, parentID)
	activity.Revision = revision
	f.mu.Lock()
	f.cache[parentID] = cachedFlatten{revision: revision, activity: activity}
	f.mu.Unlock()
	return activity
}
