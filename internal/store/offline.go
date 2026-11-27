package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const offlineSourceBytes = 16 << 20

var (
	ErrOfflineTooLarge = errors.New("会话历史超过离线保存上限")
	ErrOfflineBoundary = errors.New("无法确认完整的已结束回合")
	ErrOfflineChanged  = errors.New("保存期间历史发生变化，请重试")
)

// OfflineHistory reads one stable prefix of a local Codex rollout. It never
// asks a runtime to load the thread or treats a reconstructed tail as complete.
func (s *SessionStore) OfflineHistory(ctx context.Context, id string) (*jsoncompat.Object, string, error) {
	meta, path, err := s.historySource(id, false)
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", ErrOfflineBoundary
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, "", ErrOfflineBoundary
	}
	if before.Size() > offlineSourceBytes {
		return nil, "", ErrOfflineTooLarge
	}
	raw, err := io.ReadAll(io.NewSectionReader(file, 0, before.Size()))
	after, statErr := os.Stat(path)
	if err != nil || statErr != nil || !os.SameFile(before, after) ||
		int64(len(raw)) != before.Size() || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return nil, "", ErrOfflineChanged
	}
	end, turn, err := offlineBoundary(ctx, id, raw)
	if err != nil {
		return nil, "", err
	}
	return History(id, meta, raw[:end], nil), turn, nil
}

func offlineBoundary(ctx context.Context, sessionID string, raw []byte) (int, string, error) {
	var open, last string
	var end, offset int
	matched := false
	for len(raw) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		line, rest, terminated := bytes.Cut(raw, []byte{'\n'})
		offset += len(line)
		if terminated {
			offset++
		}
		raw = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string
			Payload json.RawMessage
		}
		if !utf8.Valid(line) || json.Unmarshal(line, &entry) != nil || entry.Type == "" {
			if !terminated && end != 0 {
				break // Codex may still be appending the final JSONL record.
			}
			return 0, "", ErrOfflineBoundary
		}
		switch entry.Type {
		case "session_meta":
			var meta struct{ ID string }
			if json.Unmarshal(entry.Payload, &meta) != nil || meta.ID == "" {
				return 0, "", ErrOfflineBoundary
			}
			// A fork may carry an earlier session_meta. The last identity before
			// a committed boundary must be the requested thread.
			matched = strings.EqualFold(meta.ID, sessionID)
		case "event_msg":
			var event struct {
				Type   string
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(entry.Payload, &event) != nil {
				return 0, "", ErrOfflineBoundary
			}
			switch event.Type {
			case "task_started":
				open = event.TurnID
				if open == "" || len(open) > 128 {
					return 0, "", ErrOfflineBoundary
				}
			case "task_complete", "turn_aborted":
				if open == "" || (event.TurnID != "" && event.TurnID != open) {
					return 0, "", ErrOfflineBoundary
				}
				if matched && event.Type == "task_complete" {
					end, last = offset, open
				}
				open = ""
			}
		}
	}
	if end == 0 || !matched {
		return 0, "", ErrOfflineBoundary
	}
	return end, last, nil
}
