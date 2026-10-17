// Structured change items for saved codex rollouts. The Python parser drops
// item_completed payloads for FileChange/CommandExecution items, and no
// conformance scenario feeds such a payload, so converting them into the
// camelCase item rows model.NormalizeItem and the httpd change annotator
// already consume is a pure addition — existing golden bytes stay untouched.
// Rollouts record these items in the older wire format: PascalCase item
// types, snake_case fields (exit_code, aggregated_output), changes as a
// path-keyed object of {type, unified_diff|content} entries, and command as
// an argv array.
package store

import (
	"strconv"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// isChangeItem reports whether an item_completed payload is a structured
// file change or command execution item.
func isChangeItem(item *jsoncompat.Object) bool {
	switch kind, _ := item.MustGet("type").(string); kind {
	case "FileChange", "fileChange", "CommandExecution", "commandExecution":
		return true
	}
	return false
}

// ChangeItemRow converts one item_completed rollout item into the item row
// the view pipeline consumes ({"type": "fileChange", "changes": [...]} /
// {"type": "commandExecution", "command", "aggregatedOutput", "exitCode"}).
// The caller guarantees isChangeItem(item). A file change whose entries all
// fail the path/text guards yields nil. fallbackIndex is the positional id
// Python-style items use when the payload carries none.
func ChangeItemRow(item *jsoncompat.Object, fallbackIndex int) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("id", changeItemID(item, fallbackIndex))
	switch kind, _ := item.MustGet("type").(string); kind {
	case "FileChange", "fileChange":
		entries := changeEntries(item)
		if len(entries.Items) == 0 {
			return nil
		}
		row.Set("type", "fileChange")
		row.Set("changes", entries)
	case "CommandExecution", "commandExecution":
		row.Set("type", "commandExecution")
		row.Set("command", changeCommandText(item))
		if output, ok := firstString(item, "aggregated_output", "aggregatedOutput"); ok {
			row.Set("aggregatedOutput", output)
		}
		if code, ok := firstValue(item, "exit_code", "exitCode"); ok {
			row.Set("exitCode", code)
		}
	}
	return row
}

// changeEntries flattens a FileChange item's changes field into the
// [{path, diff|oldText, newText}] list the view speaks. Current rollouts key
// changes by absolute path, with unified_diff entries for updates/deletes
// and content-only entries for additions; older list-shaped payloads carry
// the path inside each entry.
func changeEntries(item *jsoncompat.Object) *jsoncompat.Array {
	converted := &jsoncompat.Array{}
	switch changes := getDefault(item, "changes", &jsoncompat.Object{}).(type) {
	case *jsoncompat.Object:
		for _, path := range changes.Keys() {
			appendChangeEntry(converted, path, obj(getDefault(changes, path, &jsoncompat.Object{})))
		}
	case *jsoncompat.Array:
		for _, value := range changes.Items {
			entry := obj(value)
			if entry == nil {
				continue
			}
			path, _ := entry.MustGet("path").(string)
			appendChangeEntry(converted, path, entry)
		}
	}
	return converted
}

// appendChangeEntry copies one change into the camelCase row shape: a
// unified_diff becomes diff, otherwise content becomes an addition rendered
// through the oldText/newText pair channel. Entries without a path or any
// reviewable text are skipped, matching the httpd annotator's bounds.
func appendChangeEntry(list *jsoncompat.Array, path string, detail *jsoncompat.Object) {
	if path == "" || detail == nil {
		return
	}
	entry := jsoncompat.NewObject()
	entry.Set("path", path)
	if diff, ok := firstString(detail, "unified_diff", "unifiedDiff", "diff"); ok {
		entry.Set("diff", diff)
	} else {
		if oldText, ok := firstString(detail, "old_text", "oldText"); ok {
			entry.Set("oldText", oldText)
		}
		newText, hasNew := firstString(detail, "content", "new_text", "newText")
		if !hasNew {
			if _, hasOld := entry.Get("oldText"); !hasOld {
				return
			}
			newText = ""
		}
		entry.Set("newText", newText)
	}
	list.Items = append(list.Items, entry)
}

// changeCommandText renders the command field as the single string the view
// row shows: current rollouts record an argv array, older ones a string.
func changeCommandText(item *jsoncompat.Object) string {
	parts := []string{}
	switch command := getDefault(item, "command", &jsoncompat.Array{}).(type) {
	case *jsoncompat.Array:
		for _, value := range command.Items {
			text, _ := value.(string)
			parts = append(parts, text)
		}
	case string:
		return command
	}
	return strings.Join(parts, " ")
}

func changeItemID(item *jsoncompat.Object, fallbackIndex int) any {
	if value, ok := item.Get("id"); ok {
		return value
	}
	return strconv.Itoa(fallbackIndex)
}

// firstString returns the first present string field among the aliases.
func firstString(object *jsoncompat.Object, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := object.Get(key); ok {
			if text, isString := value.(string); isString {
				return text, true
			}
		}
	}
	return "", false
}

// firstValue returns the first present field among the aliases, keeping the
// original JSON type (exit_code stays a number).
func firstValue(object *jsoncompat.Object, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := object.Get(key); ok {
			return value, true
		}
	}
	return nil, false
}
