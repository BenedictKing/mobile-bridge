package dsh

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// ListSessions enumerates persisted DSH sessions without starting dsh-acp.
// The transcript header supplies identity and cwd; the projection cache only
// supplies the title. Rows with no valid header are skipped.
func ListSessions(root string) []*jsoncompat.Object {
	matches, err := filepath.Glob(filepath.Join(root, "sessions", "*", "*", "session.*"))
	if err != nil {
		return nil
	}
	rows := map[string]*jsoncompat.Object{}
	for _, path := range matches {
		if strings.HasSuffix(path, ".lock") {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		header := readSessionHeader(path)
		if header == nil || header.MustGet("type") != "session" {
			continue
		}
		id, _ := header.MustGet("id").(string)
		cwd, _ := header.MustGet("cwd").(string)
		if id == "" || cwd == "" || id != filepath.Base(filepath.Dir(path)) || header.MustGet("origin") == "subagent" {
			continue
		}
		updated := info.ModTime().UnixMilli()
		if previous := rows[id]; previous != nil && listUpdatedAt(previous) >= updated {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", id)
		if title := readTitle(filepath.Join(root, "storages", "session_projcache", "sessions", id+".json")); title != "" {
			row.Set("title", title)
		}
		row.Set("cwd", cwd)
		row.Set("updatedAt", jsoncompat.Num(int(updated)))
		rows[id] = row
	}
	result := make([]*jsoncompat.Object, 0, len(rows))
	for _, row := range rows {
		result = append(result, row)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return listUpdatedAt(result[i]) > listUpdatedAt(result[j])
	})
	return result
}

func listUpdatedAt(row *jsoncompat.Object) int64 {
	value, _ := row.MustGet("updatedAt").(json.Number)
	updated, _ := value.Int64()
	return updated
}

func readSessionHeader(path string) *jsoncompat.Object {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var source io.Reader = file
	if strings.HasSuffix(path, ".zstd") {
		decoder, err := zstd.NewReader(file)
		if err != nil {
			return nil
		}
		defer decoder.Close()
		source = decoder
	}
	line, err := bufio.NewReader(io.LimitReader(source, 1<<20)).ReadBytes('\n')
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	value, parseErr := jsoncompat.Unmarshal(bytes.TrimSpace(line))
	if parseErr != nil {
		return nil
	}
	header, _ := value.(*jsoncompat.Object)
	return header
}
