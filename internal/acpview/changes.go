// Structured per-file change entries on tool rows: ACP tool_call content
// arrays may carry {"type":"diff"} blocks, and edit-style rawInput payloads
// carry file_path/old_string/new_string. Both fold onto the message as a
// bounded `changes` array the timeline whitelists and the phone renders as
// per-file result cards. Blocks collected here are skipped by
// flattenContentText, so they never leak into the folded output text.
package acpview

import (
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// diffBlock extracts one ACP diff content block as a change entry. Blocks
// without a path carry no reviewable target and are dropped.
func diffBlock(block *jsoncompat.Object) *jsoncompat.Object {
	if block == nil {
		return nil
	}
	if blockType, _ := block.MustGet("type").(string); blockType != "diff" {
		return nil
	}
	path, _ := block.MustGet("path").(string)
	if path == "" {
		return nil
	}
	entry := jsoncompat.NewObject()
	entry.Set("path", path)
	for _, field := range [...]string{"oldText", "newText"} {
		if value, ok := block.Get(field); ok {
			if text, isString := value.(string); isString {
				entry.Set(field, text)
			}
		}
	}
	return entry
}

// rawInputChange rebuilds a change entry from claude-style edit rawInput
// (file_path plus old_string/new_string). Anything else returns nil.
func rawInputChange(rawInput any) *jsoncompat.Object {
	root := obj(rawInput)
	if root == nil {
		return nil
	}
	path, _ := root.MustGet("file_path").(string)
	if path == "" {
		return nil
	}
	oldText, hasOld := root.Get("old_string")
	newText, hasNew := root.Get("new_string")
	if !hasOld && !hasNew {
		return nil
	}
	entry := jsoncompat.NewObject()
	entry.Set("path", path)
	if text, ok := oldText.(string); ok {
		entry.Set("oldText", text)
	}
	if text, ok := newText.(string); ok {
		entry.Set("newText", text)
	}
	return entry
}

// appendContentChanges merges one tool update's diff content blocks onto the
// row's message, keyed by path with the newest text winning (streamed
// tool_call_update frames refine the same file). When the content array has
// no diff blocks at all, an edit-style rawInput provides the fallback entry —
// content diffs take priority, so a tool that reports both never duplicates.
func appendContentChanges(message *jsoncompat.Object, content, rawInput any) {
	list := arr(content)
	found := false
	if list != nil {
		for _, item := range list.Items {
			if change := diffBlock(obj(item)); change != nil {
				mergeChange(message, change)
				found = true
			}
		}
	}
	if !found {
		if change := rawInputChange(rawInput); change != nil {
			mergeChange(message, change)
		}
	}
}

// mergeChange upserts one entry into the message's changes array, replacing
// an existing same-path entry in place (first-seen position, latest text).
func mergeChange(message *jsoncompat.Object, change *jsoncompat.Object) {
	path, _ := change.MustGet("path").(string)
	changes, _ := message.MustGet("changes").(*jsoncompat.Array)
	if changes == nil {
		changes = &jsoncompat.Array{}
		message.Set("changes", changes)
	}
	for index, existingValue := range changes.Items {
		existing := obj(existingValue)
		if existing == nil {
			continue
		}
		if existingPath, _ := existing.MustGet("path").(string); existingPath == path {
			changes.Items[index] = change
			return
		}
	}
	changes.Items = append(changes.Items, change)
}
