// Structured change annotation for codex views: the model normalization
// (locked by conformance goldens) folds fileChange diffs into row text and
// drops commandExecution exit codes, so this file re-attaches both as
// structured fields on the normalized view messages the timeline
// whitelists. Runs on the request-private view before timeline.Update,
// mirroring the enrichTimeline precedent of post-processing at the httpd
// layer instead of touching locked normalizers. ACP views arrive with
// `changes` already set by acpview and skip this path.
package httpd

import (
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

const (
	// changesMaxFiles caps the per-item entry list; changesMaxChars caps one
	// entry's diff text. Both match the timeline's own row bounds.
	changesMaxFiles = 20
	changesMaxChars = 16000
)

// annotateViewChanges copies per-item results (file diffs and command exit
// codes) from raw codex turns onto the matching normalized view messages.
// NormalizeState emits one view turn per raw turn in order, and one message
// per non-merged item keyed by the item id, so an id map resolves rows
// without relying on adjacency. Values are cloned and bounded: the view is
// request-private but shares leaf values with the follower state.
func annotateViewChanges(view *jsoncompat.Object, turns *jsoncompat.Array) {
	if view == nil || turns == nil || len(turns.Items) == 0 {
		return
	}
	viewTurns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if viewTurns == nil {
		return
	}
	count := len(turns.Items)
	if len(viewTurns.Items) < count {
		count = len(viewTurns.Items)
	}
	for index := 0; index < count; index++ {
		rawTurn, _ := turns.Items[index].(*jsoncompat.Object)
		viewTurn, _ := viewTurns.Items[index].(*jsoncompat.Object)
		if rawTurn == nil || viewTurn == nil {
			continue
		}
		annotateTurnChanges(rawTurn, viewTurn)
	}
}

func annotateTurnChanges(rawTurn, viewTurn *jsoncompat.Object) {
	type itemResult struct {
		changes  *jsoncompat.Array
		exitCode any
	}
	results := map[string]*itemResult{}
	items := model.ItemsArray(getDefaultObject(rawTurn, "items"))
	messages, _ := viewTurn.MustGet("messages").(*jsoncompat.Array)
	if len(items.Items) == 0 || messages == nil {
		return
	}
	for _, itemValue := range items.Items {
		item, _ := itemValue.(*jsoncompat.Object)
		if item == nil {
			continue
		}
		id, _ := item.MustGet("id").(string)
		if id == "" {
			continue
		}
		kind, _ := item.MustGet("type").(string)
		switch kind {
		case "fileChange":
			changes := boundedChanges(item.MustGet("changes"))
			if changes == nil {
				continue
			}
			entry := results[id]
			if entry == nil {
				entry = &itemResult{}
				results[id] = entry
			}
			entry.changes = changes
		case "commandExecution":
			exitCode, exists := item.Get("exitCode")
			if !exists {
				continue
			}
			entry := results[id]
			if entry == nil {
				entry = &itemResult{}
				results[id] = entry
			}
			entry.exitCode = exitCode
		}
	}
	if len(results) == 0 {
		return
	}
	for _, messageValue := range messages.Items {
		message, _ := messageValue.(*jsoncompat.Object)
		if message == nil {
			continue
		}
		id, _ := message.MustGet("id").(string)
		entry := results[id]
		if entry == nil {
			continue
		}
		if entry.changes != nil {
			message.Set("changes", entry.changes)
		}
		if entry.exitCode != nil {
			message.Set("exitCode", entry.exitCode)
		}
	}
}

// boundedChanges clones a codex item's [{path,diff}] list into the bounded
// shape the timeline and the phone renderer expect: at most 20 files, each
// diff sliced to 16000 chars with a truncated marker. Nil when there is
// nothing reviewable.
func boundedChanges(value any) *jsoncompat.Array {
	source, _ := value.(*jsoncompat.Array)
	if source == nil || len(source.Items) == 0 {
		return nil
	}
	limit := len(source.Items)
	if limit > changesMaxFiles {
		limit = changesMaxFiles
	}
	converted := &jsoncompat.Array{}
	for _, item := range source.Items[:limit] {
		change, _ := item.(*jsoncompat.Object)
		if change == nil {
			continue
		}
		path, _ := change.MustGet("path").(string)
		if path == "" {
			continue
		}
		entry := jsoncompat.NewObject()
		entry.Set("path", clipRunes(path, 300))
		if diff, ok := change.Get("diff"); ok {
			if text, isString := diff.(string); isString {
				entry.Set("diff", clipRunes(text, changesMaxChars))
				if len([]rune(text)) > changesMaxChars {
					entry.Set("truncated", true)
				}
			}
		}
		converted.Items = append(converted.Items, entry)
	}
	if len(converted.Items) == 0 {
		return nil
	}
	return converted
}

func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func getDefaultObject(object *jsoncompat.Object, key string) any {
	if value, ok := object.Get(key); ok {
		return value
	}
	return &jsoncompat.Array{}
}
