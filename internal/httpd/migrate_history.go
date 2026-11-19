package httpd

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// A Markdown reference definition stays invisible in chat while recording
// exact message boundaries. Legacy transcripts have no such framing.
const migrationHistoryFramePrefix = "[mobile-bridge-transcript-v1]: # \""

func migrationHistoryFrame(entries []migrationEntry, instructions string) string {
	fields := make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		fields = append(fields, entry.role+":"+strconv.Itoa(len(entry.text)))
	}
	if instructions != "" {
		fields = append(fields, "instructions:"+strconv.Itoa(len(instructions)))
	}
	return migrationHistoryFramePrefix + strings.Join(fields, " ") + "\""
}

func decodeMigrationHistory(text string) ([]migrationEntry, string, bool) {
	header, body, found := strings.Cut(text, "\n")
	fields, framed := strings.CutPrefix(header, migrationHistoryFramePrefix)
	fields, closed := strings.CutSuffix(fields, "\"")
	if !found || !framed || !closed || !strings.HasPrefix(body, "\n") {
		return nil, "", false
	}
	body = strings.TrimPrefix(body, "\n")
	entries := []migrationEntry{}
	instructionsBytes := -1
	for _, field := range strings.Fields(fields) {
		role, countText, valid := strings.Cut(field, ":")
		count, err := strconv.Atoi(countText)
		if !valid || err != nil || count < 1 || instructionsBytes >= 0 {
			return nil, "", false
		}
		if role == "instructions" {
			instructionsBytes = count
			continue
		}
		nextRole, remaining, valid := migrationHistoryRole(body)
		if !valid || nextRole != role || count > len(remaining) || !utf8.ValidString(remaining[:count]) || strings.TrimSpace(remaining[:count]) != remaining[:count] || !strings.HasPrefix(remaining[count:], "\n") {
			return nil, "", false
		}
		entries = append(entries, migrationEntry{role, remaining[:count]})
		body = remaining[count+1:]
	}
	footer, remaining, _ := strings.Cut(strings.TrimPrefix(body, "\n"), "\n")
	if !migrationHistoryFooter(footer) {
		return nil, "", false
	}
	if instructionsBytes < 0 {
		return entries, remaining, true
	}
	for _, texts := range migrationTextsByLocale {
		instructions, found := strings.CutPrefix(remaining, "\n"+texts.instructions+"\n")
		if found && len(instructions) >= instructionsBytes && utf8.ValidString(instructions[:instructionsBytes]) {
			tail := instructions[instructionsBytes:]
			if tail == "" || strings.HasPrefix(tail, "\n") {
				return append(entries, migrationEntry{"user", instructions[:instructionsBytes]}), strings.TrimPrefix(tail, "\n"), true
			}
		}
	}
	return nil, "", false
}

// extractMigrationHistory unwraps only gateway-generated user messages. Scan
// nested envelopes in one pass before applying budgets, so migration headers
// never replace the original task or consume the dialogue's message slots.
func extractMigrationHistory(entry migrationEntry) ([]migrationEntry, bool) {
	if entry.role != "user" {
		return []migrationEntry{entry}, false
	}
	lines := strings.Split(entry.text, "\n")
	index, truncated, ok := migrationHistoryStart(lines, 0, lines[0])
	if !ok {
		return []migrationEntry{entry}, false
	}
	entries := []migrationEntry{}
	role := ""
	parts := []string{}
	flush := func() {
		if text := strings.TrimSpace(strings.Join(parts, "\n")); role != "" && text != "" {
			entries = append(entries, migrationEntry{role, text})
		}
		role, parts = "", nil
	}
	fence := migrationHistoryFence{}
	for index < len(lines) {
		line := strings.TrimSuffix(lines[index], "\r")
		if fence.width == 0 {
			if strings.HasPrefix(line, migrationHistoryFramePrefix) {
				history, remaining, valid := decodeMigrationHistory(strings.Join(lines[index:], "\n"))
				if !valid {
					// A damaged frame must not split body examples into messages.
					return []migrationEntry{entry}, truncated
				}
				flush()
				entries = append(entries, history...)
				index = len(lines)
				if remaining != "" {
					index -= len(strings.Split(remaining, "\n"))
				}
				continue
			}
			if nextRole, text, found := migrationHistoryRole(line); found {
				flush()
				if nextRole == "user" {
					if next, clipped, nested := migrationHistoryStart(lines, index, text); nested {
						truncated = truncated || clipped
						index = next
						continue
					}
				}
				role, parts = nextRole, []string{text}
				fence.update(text)
				index++
				continue
			}
			if migrationHistoryFooter(line) {
				flush()
				index++
				continue
			}
			if migrationHistoryInstructions(line) {
				flush()
				role = "user"
				index++
				continue
			}
		}
		if role == "" && strings.TrimSpace(line) != "" {
			// Preserve unexpected text rather than silently discarding it.
			role = "user"
		}
		if role != "" {
			parts = append(parts, lines[index])
			fence.update(line)
		}
		index++
	}
	flush()
	return entries, truncated
}

func migrationHistoryStart(lines []string, index int, header string) (int, bool, bool) {
	if index+2 >= len(lines) {
		return index, false, false
	}
	header = strings.TrimSuffix(header, "\r")
	for _, texts := range migrationTextsByLocale {
		if !migrationHistoryTemplate(header, texts.header) && !migrationHistoryTemplate(header, texts.latestHeader) {
			continue
		}
		if !strings.HasPrefix(lines[index+1], texts.title) || !strings.HasPrefix(lines[index+2], texts.directory) {
			return index, false, false
		}
		next := index + 3
		truncated := next < len(lines) && strings.TrimSuffix(lines[next], "\r") == texts.truncated
		if truncated {
			next++
		}
		for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
			next++
		}
		return next, truncated, true
	}
	return index, false, false
}

func migrationHistoryTemplate(line, template string) bool {
	prefix, suffix, found := strings.Cut(template, "%s")
	return found && len(line) > len(prefix)+len(suffix) && strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix)
}

func migrationHistoryRole(line string) (string, string, bool) {
	for _, texts := range migrationTextsByLocale {
		for role, label := range texts.roles {
			if text, found := strings.CutPrefix(line, label+texts.sep); found {
				return role, text, true
			}
		}
	}
	return "", "", false
}

func migrationHistoryFooter(line string) bool {
	for _, texts := range migrationTextsByLocale {
		if migrationHistoryTemplate(line, texts.cont) || migrationHistoryTemplate(line, texts.latestContinue) {
			return true
		}
	}
	return false
}

func migrationHistoryInstructions(line string) bool {
	for _, texts := range migrationTextsByLocale {
		if line == texts.instructions {
			return true
		}
	}
	return false
}

// Role labels and envelope-looking text in fenced examples are message content.
type migrationHistoryFence struct {
	marker byte
	width  int
}

func (f *migrationHistoryFence) update(line string) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return
	}
	width := 0
	for width < len(trimmed) && trimmed[width] == trimmed[0] {
		width++
	}
	if f.width == 0 && width >= 3 {
		f.marker, f.width = trimmed[0], width
	} else if f.marker == trimmed[0] && width >= f.width && strings.TrimSpace(trimmed[width:]) == "" {
		f.marker, f.width = 0, 0
	}
}
