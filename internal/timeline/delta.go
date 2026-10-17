// Delta encoding of pure-append text growth, layered on top of the
// byte-locked Page/Changes/Detail contract. Deltas is additive: no existing
// method changes shape, and any row move that is not a plain text append
// makes Deltas report false so callers fall back to Changes.
package timeline

import (
	"encoding/json"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Deltas describes the row moves between sequence `after` and the current
// one when every changed row is a pure text append against the immediately
// previous generation (after == the previous sequence): same key, old text a
// strict prefix of the new text, both sides inside the untruncated preview
// budget, every other record field and the row order untouched, and each
// appended tail within deltaMaxAppend bytes. The payload carries {epoch,
// sequence, rows:[{key, version, prevVersion, append}], meta}; ok is false
// whenever the move is not representable this way and the caller must fall
// back to Changes.
func (t *Timeline) Deltas(after string) (*jsoncompat.Object, bool) {
	if after == "" || after != t.prevSequence {
		return nil, false
	}
	previous, ok := t.versions[after]
	if !ok {
		return nil, false
	}
	current := t.versions[t.sequence]
	rows := &jsoncompat.Array{}
	for _, record := range t.rows {
		previousDigest, existed := previous[record.key]
		if existed && previousDigest == current[record.key] {
			continue
		}
		if !existed || record.appendText == "" {
			return nil, false
		}
		item := jsoncompat.NewObject()
		item.Set("key", record.key)
		item.Set("version", record.version)
		item.Set("prevVersion", record.prevVersion)
		item.Set("append", record.appendText)
		rows.Items = append(rows.Items, item)
	}
	result := jsoncompat.NewObject()
	result.Set("epoch", t.epoch)
	result.Set("sequence", json.Number(t.sequence))
	result.Set("rows", rows)
	result.Set("meta", jsoncompat.Clone(t.meta))
	return result, true
}
