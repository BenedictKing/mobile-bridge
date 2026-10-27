package jev

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// journalMaxBytes is the rotation point for jev-decisions.jsonl: past it the
// file is renamed to .1 (overwriting any older .1) and a fresh file starts.
const journalMaxBytes = 20 << 20

// SubjectPrefix picks the human-recognizable descriptor of a permission card
// for logs and the decision journal: the title in full up to 60 runes, the
// command otherwise as a prefix so the log never carries a complete command
// line. Raw ACP cards get both through PermissionCard; its placeholder title
// for title-less tool calls is not a real subject, so those fall through to
// the command.
func SubjectPrefix(params *jsoncompat.Object) string {
	subject, _ := subjectText(params)
	if subject == "" {
		return ""
	}
	return truncateRunes(subject, 60)
}

// subjectText returns the card's subject material (title first, command
// otherwise) and whether the card is really a question card.
func subjectText(params *jsoncompat.Object) (string, bool) {
	if params == nil {
		return "", false
	}
	title, command, question := subjectOf(params)
	if title == permissionRequestTitle {
		title = ""
	}
	if title != "" {
		return title, question
	}
	return command, question
}

// journalPath is the JSONL decision journal, kept beside jev.json.
func (s *Settings) journalPath() string {
	return filepath.Join(filepath.Dir(s.path), "jev-decisions.jsonl")
}

// recordDecision appends one JSONL line for a decision that entered the
// evaluation chain. The pre-evaluation early exits (auto-approve off, no key,
// unsupported method) never touch the journal: a gateway that never enabled
// Jev must not grow a decisions file. Any write failure is logged and
// swallowed — the approval flow must never depend on the journal.
func (s *Settings) recordDecision(agent, sessionID, requestID, method string, d Decision, params *jsoncompat.Object) {
	switch d.Reason {
	case "auto_approve_off", "no_api_key", "unsupported_method":
		return
	}
	line := jsoncompat.NewObject()
	line.Set("time", time.Now().Format(time.RFC3339))
	line.Set("agent", agent)
	line.Set("session", sessionID)
	line.Set("request", requestID)
	line.Set("method", method)
	line.Set("reason", d.Reason)
	categories := jsoncompat.NewObject()
	names := make([]string, 0, len(d.Categories))
	for name := range d.Categories {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		categories.Set(name, numFloat(d.Categories[name]))
	}
	line.Set("categories", categories)
	line.Set("negative_sum", numFloat(d.NegativeSum))
	line.Set("threshold", numFloat(d.Threshold))
	line.Set("reversible", numFloat(d.Reversible))
	line.Set("confidence", numFloat(d.Confidence))
	line.Set("recommend", d.Recommend)
	line.Set("subject", SubjectPrefix(params))
	encoded := jsoncompat.EncodeCompact(line)
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	path := s.journalPath()
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(encoded))+1 > journalMaxBytes {
		_ = os.Remove(path + ".1")
		if err := os.Rename(path, path+".1"); err != nil {
			log.Printf("Jev 决策日志写入失败: %v", err)
			return
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("Jev 决策日志写入失败: %v", err)
		return
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		log.Printf("Jev 决策日志写入失败: %v", err)
	}
	_ = file.Close()
}
