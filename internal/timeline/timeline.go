// Package timeline is the Go port of bridge/timeline.py: bounded phone
// projections of desktop agent state. Cursor semantics, row key derivation,
// page budgets and the version window replicate the Python implementation
// exactly; conformance is enforced by byte-comparing replayed scenarios
// against the Python oracle's golden output (scenario_test.go).
package timeline

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	PageBytes       = 192 * 1024
	TextPreview     = 8000
	DetailChars     = 16000
	activityPreview = 180
	maxPageLimit    = 100
	versionWindow   = 16
	changesMaxRows  = 100
	errStaleCursor  = "这条内容已更新，请重新打开聊天"
)

// rowWhitelist is copied from the source message in this order.
var rowWhitelist = [...]string{
	"role", "kind", "status", "title", "phase", "requestId",
	"subagentId", "subagentType", "subagentDescription", "subagentPrompt", "stopReason",
}

type row struct {
	obj     *jsoncompat.Object
	enc     []byte // encoded(row), cached: page budget cost and change digests
	digest  [32]byte
	key     string
	version string
	text    string // details[key]: the untruncated text
}

// Timeline mirrors bridge.timeline.Timeline. sequence is kept as the original
// number lexeme so serialization and map keys match Python's int handling.
type Timeline struct {
	epoch       string
	sequence    string
	orderOrigin int
	rows        []*row
	positions   map[string]int
	details     map[string]string
	versionKeys []string // insertion order of versions
	versions    map[string]map[string][32]byte
	meta        *jsoncompat.Object
	newEpoch    func() string
}

// New creates a Timeline. epochFn supplies uuid4-hex epochs; inject a
// deterministic source in tests.
func New(epochFn func() string) *Timeline {
	if epochFn == nil {
		epochFn = randomEpoch
	}
	return &Timeline{
		epoch:     epochFn(),
		sequence:  "-1",
		positions: map[string]int{},
		details:   map[string]string{},
		versions:  map[string]map[string][32]byte{},
		meta:      jsoncompat.NewObject(),
		newEpoch:  epochFn,
	}
}

func randomEpoch() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}

func hash24(value any) string {
	sum := sha256.Sum256(jsoncompat.EncodeCompact(value))
	return hex.EncodeToString(sum[:])[:24]
}

func hash24Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:24]
}

// runeLen mirrors Python len() on strings (code points, not bytes).
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// runeSlice mirrors Python s[start:end] for non-negative bounds.
func runeSlice(s string, start, end int) string {
	var builder strings.Builder
	count := 0
	for _, r := range s {
		if count >= end {
			break
		}
		if count >= start {
			builder.WriteRune(r)
		}
		count++
	}
	return builder.String()
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// Update rebuilds the projection from a normalized view.
func (t *Timeline) Update(view *jsoncompat.Object) {
	sequence, _ := view.MustGet("sequence").(json.Number)
	lexeme := sequence.String()
	if lexeme == t.sequence {
		return
	}
	activation := map[string]bool{}
	if ids, ok := view.MustGet("goalActivationIds").(*jsoncompat.Array); ok {
		for _, item := range ids.Items {
			if id, ok := item.(string); ok {
				activation[id] = true
			}
		}
	}
	rows := []*row{}
	details := map[string]string{}
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns != nil {
		for _, turnValue := range turns.Items {
			turn, _ := turnValue.(*jsoncompat.Object)
			if turn == nil {
				continue
			}
			turnID := turn.MustGet("id")
			messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
			if messages == nil {
				messages = &jsoncompat.Array{}
			}
			occurrences := map[string]int{}
			firstUser := -1
			for index, messageValue := range messages.Items {
				message, _ := messageValue.(*jsoncompat.Object)
				if message == nil {
					continue
				}
				if role, _ := message.MustGet("role").(string); role == "user" {
					firstUser = index
					break
				}
			}
			for index, messageValue := range messages.Items {
				message, _ := messageValue.(*jsoncompat.Object)
				if message == nil {
					continue
				}
				if requestID, ok := message.MustGet("requestId").(string); ok &&
					requestID != "" && activation[requestID] {
					continue
				}
				identity := &jsoncompat.Array{Items: []any{
					turnID, message.MustGet("id"), message.MustGet("kind")}}
				base := hash24(identity)
				ordinal := occurrences[base]
				occurrences[base] = ordinal + 1
				key := base + "-" + strconv.Itoa(ordinal)
				text, _ := message.MustGet("text").(string)
				if output, ok := message.MustGet("output").(string); ok && output != "" {
					text += "\n\n" + output
				}
				role, _ := message.MustGet("role").(string)
				activity := role == "activity"
				version := hash24Text(text)
				record := jsoncompat.NewObject()
				for _, field := range rowWhitelist {
					if value, ok := message.Get(field); ok {
						record.Set(field, value)
					}
				}
				if entries, ok := message.Get("entries"); ok && jsoncompat.Truthy(entries) {
					record.Set("entries", entries)
				}
				record.Set("key", key)
				record.Set("turnId", turnID)
				record.Set("order", jsoncompat.Num(len(rows)))
				record.Set("version", version)
				previewLimit := TextPreview
				if activity {
					previewLimit = activityPreview
				}
				record.Set("text", runeSlice(text, 0, previewLimit))
				record.Set("truncated", (activity && text != "") || runeLen(text) > TextPreview)
				record.Set("turnStatus", turn.MustGet("status"))
				record.Set("editable", editableValue(turn, index == firstUser, message))
				record.Set("forkable", forkableValue(turn, message))
				if attachments, ok := message.Get("attachments"); ok && jsoncompat.Truthy(attachments) {
					if source, ok := attachments.(*jsoncompat.Array); ok {
						converted := &jsoncompat.Array{}
						limit := len(source.Items)
						if limit > 20 {
							limit = 20
						}
						for _, item := range source.Items[:limit] {
							attachment, _ := item.(*jsoncompat.Object)
							if attachment == nil {
								continue
							}
							entry := jsoncompat.NewObject()
							for _, field := range [...]string{"type", "name", "path"} {
								if value, ok := attachment.Get(field); ok {
									entry.Set(field, runeSlice(jsoncompat.PyStr(value), 0, 300))
								}
							}
							converted.Items = append(converted.Items, entry)
						}
						record.Set("attachments", converted)
					}
				}
				rows = append(rows, &row{obj: record, key: key, version: version, text: text})
				details[key] = text
			}
			if failure, ok := turn.Get("error"); ok && jsoncompat.Truthy(failure) {
				text, isString := failure.(string)
				if !isString {
					text = string(jsoncompat.EncodePythonDefault(failure))
				}
				errorIdentity := &jsoncompat.Array{Items: []any{turnID, "error"}}
				key := hash24(errorIdentity) + "-error"
				record := jsoncompat.NewObject()
				record.Set("key", key)
				record.Set("turnId", turnID)
				record.Set("order", jsoncompat.Num(len(rows)))
				record.Set("role", "error")
				record.Set("text", runeSlice(text, 0, TextPreview))
				record.Set("truncated", runeLen(text) > TextPreview)
				record.Set("version", hash24Text(text))
				rows = append(rows, &row{obj: record, key: key, version: hash24Text(text), text: text})
				details[key] = text
			}
		}
	}

	oldKeys := make([]string, len(t.rows))
	for index, record := range t.rows {
		oldKeys[index] = record.key
	}
	newKeys := make([]string, len(rows))
	for index, record := range rows {
		newKeys[index] = record.key
	}
	first := 0
	if len(oldKeys) > 0 {
		for index, key := range newKeys {
			if key == oldKeys[0] {
				first = index
				break
			}
		}
		matches := first+len(oldKeys) <= len(newKeys)
		if matches {
			for index, key := range oldKeys {
				if newKeys[first+index] != key {
					matches = false
					break
				}
			}
		}
		if !matches {
			// Source history was replaced or reordered: invalidate all cursors.
			t.epoch = t.newEpoch()
			t.versionKeys = nil
			t.versions = map[string]map[string][32]byte{}
			t.orderOrigin = 0
		} else {
			// Prepending saved turns preserves existing row keys and cursors.
			t.orderOrigin -= first
		}
	}
	for index, record := range rows {
		record.obj.Set("order", jsoncompat.Num(t.orderOrigin+index))
		record.enc = jsoncompat.EncodeCompact(record.obj)
		record.digest = sha256.Sum256(record.enc)
	}

	t.sequence = lexeme
	t.rows = rows
	t.details = details
	t.positions = make(map[string]int, len(rows))
	for index, record := range rows {
		t.positions[record.key] = index
	}
	meta := jsoncompat.NewObject()
	for _, key := range view.Keys() {
		if key == "turns" {
			continue
		}
		value, _ := view.Get(key)
		meta.Set(key, value)
	}
	meta.Set("latestUserTurnId", latestUserTurnID(turns, activation))
	t.meta = meta
	digests := make(map[string][32]byte, len(rows))
	for _, record := range rows {
		digests[record.key] = record.digest
	}
	if _, exists := t.versions[lexeme]; !exists {
		t.versionKeys = append(t.versionKeys, lexeme)
	}
	t.versions[lexeme] = digests
	for len(t.versionKeys) > versionWindow {
		delete(t.versions, t.versionKeys[0])
		t.versionKeys = t.versionKeys[1:]
	}
}

// editableValue reproduces `turn.get('actionable', True) and message is
// first_user and message.get('kind') == 'userMessage'`, including Python's
// and-chain value semantics (a falsy actionable short-circuits to itself).
func editableValue(turn *jsoncompat.Object, isFirstUser bool, message *jsoncompat.Object) any {
	actionable, ok := turn.Get("actionable")
	if !ok {
		actionable = true
	}
	if !jsoncompat.Truthy(actionable) {
		return actionable
	}
	if !isFirstUser {
		return false
	}
	kind, _ := message.MustGet("kind").(string)
	return kind == "userMessage"
}

// forkableValue reproduces `turn.get('actionable', True) and role ==
// 'assistant' and phase != 'commentary' and turn.get('status') ==
// 'completed'`.
func forkableValue(turn *jsoncompat.Object, message *jsoncompat.Object) any {
	actionable, ok := turn.Get("actionable")
	if !ok {
		actionable = true
	}
	if !jsoncompat.Truthy(actionable) {
		return actionable
	}
	role, _ := message.MustGet("role").(string)
	if role != "assistant" {
		return false
	}
	if phase, _ := message.MustGet("phase").(string); phase == "commentary" {
		return false
	}
	status, _ := turn.MustGet("status").(string)
	return status == "completed"
}

func latestUserTurnID(turns *jsoncompat.Array, activation map[string]bool) any {
	if turns == nil {
		return nil
	}
	for i := len(turns.Items) - 1; i >= 0; i-- {
		turn, _ := turns.Items[i].(*jsoncompat.Object)
		if turn == nil {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, messageValue := range messages.Items {
			message, _ := messageValue.(*jsoncompat.Object)
			if message == nil {
				continue
			}
			role, _ := message.MustGet("role").(string)
			requestID, hasRequest := message.MustGet("requestId").(string)
			if role == "user" && (!hasRequest || requestID == "" || !activation[requestID]) {
				return turn.MustGet("id")
			}
		}
	}
	return nil
}

// Cursor builds an opaque cursor for a row key.
func (t *Timeline) Cursor(key string) string { return t.epoch + "." + key }

func (t *Timeline) position(cursor string) (int, bool) {
	epoch, key, _ := strings.Cut(cursor, ".")
	if epoch != t.epoch {
		return 0, false
	}
	index, ok := t.positions[key]
	return index, ok
}

// Page returns up to limit rows ending at the row before `before` (empty
// means the newest end), bounded by PAGE_BYTES.
func (t *Timeline) Page(limit int, before string) *jsoncompat.Object {
	end := len(t.rows)
	if before != "" {
		position, ok := t.position(before)
		if !ok {
			return t.withReset(t.Page(20, ""))
		}
		end = position
	}
	limit = clamp(limit, 1, maxPageLimit)
	start := end - limit
	if start < 0 {
		start = 0
	}
	window := t.rows[start:end]
	selected := []*row{}
	size := 0
	for i := len(window) - 1; i >= 0; i-- {
		cost := len(window[i].enc)
		if len(selected) > 0 && size+cost > PageBytes {
			break
		}
		selected = append(selected, window[i])
		size += cost
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	startIndex := end - len(selected)
	result := jsoncompat.NewObject()
	result.Set("epoch", t.epoch)
	result.Set("sequence", json.Number(t.sequence))
	rowValues := &jsoncompat.Array{}
	for _, record := range selected {
		rowValues.Items = append(rowValues.Items, record.obj)
	}
	result.Set("rows", rowValues)
	if len(selected) > 0 {
		result.Set("before", t.Cursor(selected[0].key))
	} else {
		result.Set("before", nil)
	}
	result.Set("hasMore", startIndex > 0 || jsoncompat.Truthy(t.meta.MustGet("savedHistoryMore")))
	result.Set("meta", t.meta)
	return result
}

// Changes returns rows whose content digest moved between sequence `after`
// and the current one. Anything unresolvable falls back to a reset page.
func (t *Timeline) Changes(after, epoch, start string) *jsoncompat.Object {
	first := -1
	if start != "" {
		position, ok := t.position(start)
		if !ok {
			return t.withReset(t.Page(20, ""))
		}
		first = position
	}
	if epoch != t.epoch {
		return t.withReset(t.Page(20, ""))
	}
	previous, ok := t.versions[after]
	if !ok {
		return t.withReset(t.Page(20, ""))
	}
	current := t.versions[t.sequence]
	from := 0
	if first >= 0 {
		from = first
	}
	changed := &jsoncompat.Array{}
	size := 0
	for _, record := range t.rows[from:] {
		previousDigest, existed := previous[record.key]
		if !existed || previousDigest != current[record.key] {
			changed.Items = append(changed.Items, record.obj)
			size += len(record.enc)
		}
	}
	if len(changed.Items) > changesMaxRows || size > PageBytes {
		return t.withReset(t.Page(20, ""))
	}
	result := jsoncompat.NewObject()
	result.Set("epoch", t.epoch)
	result.Set("sequence", json.Number(t.sequence))
	result.Set("rows", changed)
	result.Set("meta", t.meta)
	return result
}

// Detail returns one DETAIL_CHARS chunk of a row's full text. A mismatched
// version forces the offset back to 0.
func (t *Timeline) Detail(cursor string, offset int, version any) (*jsoncompat.Object, error) {
	position, ok := t.position(cursor)
	if !ok {
		return nil, errors.New(errStaleCursor)
	}
	record := t.rows[position]
	text := t.details[record.key]
	if expected, ok := version.(string); ok && expected == record.version {
		offset = clamp(offset, 0, runeLen(text))
	} else {
		offset = 0
	}
	end := runeLen(text)
	if end > offset+DetailChars {
		end = offset + DetailChars
	}
	result := jsoncompat.NewObject()
	result.Set("key", record.key)
	result.Set("version", record.version)
	result.Set("text", runeSlice(text, offset, end))
	result.Set("offset", jsoncompat.Num(offset))
	if end < runeLen(text) {
		result.Set("next", jsoncompat.Num(end))
	} else {
		result.Set("next", nil)
	}
	return result, nil
}

// State exposes internals used by the conformance scenarios only.
type State struct {
	Epoch    string
	Sequence string
	Versions int
	Rows     int
}

func (t *Timeline) State() State {
	return State{Epoch: t.epoch, Sequence: t.sequence,
		Versions: len(t.versionKeys), Rows: len(t.rows)}
}

func (t *Timeline) withReset(page *jsoncompat.Object) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for _, key := range page.Keys() {
		value, _ := page.Get(key)
		result.Set(key, value)
	}
	result.Set("reset", true)
	return result
}
