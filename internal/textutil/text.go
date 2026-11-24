// Package textutil contains display-only text cleanup shared by agent views.
package textutil

import (
	"regexp"
	"strings"
)

// internalReminder also matches attribute-carrying reminders such as the
// CodeBuddy/Claude local-command caveat `<system-reminder data-role="command-caveat">`.
var internalReminder = regexp.MustCompile(`(?s)<system-reminder(?:\s[^>]*)?>.*?</system-reminder>`)

// environmentContext matches the Codex harness `<environment_context>` blocks
// replayed as user messages (session start, day rollover); they are runtime
// metadata, never a user question.
var environmentContext = regexp.MustCompile(`(?s)<environment_context(?:\s[^>]*)?>.*?</environment_context>`)

// localCommandBlock matches the slash-command wrappers Claude-style CLIs
// persist as user messages; none of them is a user question, so titles skip them.
var localCommandBlock = regexp.MustCompile(`(?s)<(command-name|command-message|command-args|local-command-stdout|local-command-stderr)>.*?</(?:command-name|command-message|command-args|local-command-stdout|local-command-stderr)>`)

// Team dispatch envelopes (internal/team context.go / actions.go / review.go).
// Kept as literals so textutil stays a leaf package; team tests pin them.
const assignedTaskHeader = "[Assigned task]"

// teamEnvelopes 是团队派发信封的头与编排器固定文案：只有头后紧跟固定文案才算
// 信封，用户正文以「[Team review] …」开头时原样保留。
var teamEnvelopes = []struct{ header, marker string }{
	{"[Team context]", "来源成员："},
	{"[Team delegation]", "你是本次自动规划的 Lead"},
	{"[Team review]", "你是本团队的评审成员"},
}

// Migration and fork prompts (internal/httpd migrate.go / acp_fork.go). Agents
// sometimes derive a session title from these synthetic headers; display the
// source title carried by the envelope instead of the protocol text itself.
var transcriptEnvelopes = []struct{ prefix, suffix string }{
	{"【从 ", " 迁移的会话】"},
	{"【来自 ", " 的最新问答】"},
	{"【从 ", " 分支的会话】"},
	{"[Session migrated from ", "]"},
	{"[Latest exchange from ", "]"},
	{"[Session branched from ", "]"},
}

func transcriptTitle(text string) (string, bool) {
	for _, envelope := range transcriptEnvelopes {
		if !strings.HasPrefix(text, envelope.prefix) {
			continue
		}
		end := strings.Index(text, envelope.suffix)
		if end < 0 {
			continue
		}
		if newline := strings.Index(text, "\n"); newline >= 0 && end > newline {
			continue
		}
		rest := strings.TrimSpace(text[end+len(envelope.suffix):])
		for _, label := range []string{"标题：", "Title:"} {
			if !strings.HasPrefix(rest, label) {
				continue
			}
			title := strings.TrimSpace(strings.TrimPrefix(rest, label))
			if line, _, found := strings.Cut(title, "\n"); found {
				title = strings.TrimSpace(line)
			}
			for _, directory := range []string{" 目录：", " Directory:"} {
				if at := strings.Index(title, directory); at >= 0 {
					title = strings.TrimSpace(title[:at])
				}
			}
			return title, true
		}
		if rest == "" || strings.HasPrefix(rest, "目录：") || strings.HasPrefix(rest, "Directory:") {
			return "", true
		}
	}
	return text, false
}

// StripInternalReminders removes harness-only reminder envelopes from the
// phone timeline. It never changes prompts sent to an agent.
func StripInternalReminders(text string) string {
	cleaned := internalReminder.ReplaceAllString(text, "")
	cleaned = environmentContext.ReplaceAllString(cleaned, "")
	if cleaned == text {
		return text
	}
	return strings.TrimSpace(cleaned)
}

// titleBody strips protocol wrappers before a title is derived: internal
// reminders, local-command blocks, Team envelopes (the real task follows the
// last `[Assigned task]` header), and migration/fork transcript envelopes.
func titleBody(text string) string {
	text = strings.TrimSpace(localCommandBlock.ReplaceAllString(StripInternalReminders(text), ""))
	for _, envelope := range teamEnvelopes {
		if strings.HasPrefix(text, envelope.header) && strings.HasPrefix(strings.TrimSpace(text[len(envelope.header):]), envelope.marker) {
			if at := strings.LastIndex(text, "\n"+assignedTaskHeader+"\n"); at >= 0 {
				return strings.TrimSpace(text[at+len(assignedTaskHeader)+2:])
			}
			return ""
		}
	}
	if title, ok := transcriptTitle(text); ok {
		return title
	}
	return text
}

// CleanTitle sanitizes an existing list-row title: titles carrying protocol
// wrappers are rewritten via TitleFromText, ordinary titles (user renames
// included) are returned untouched and untruncated. "" means protocol-only;
// callers fall back to another title source.
func CleanTitle(title string) string {
	if titleBody(title) != strings.TrimSpace(title) {
		return TitleFromText(title)
	}
	return title
}

// TitleFromText turns conversation content into a compact display title.
func TitleFromText(text string) string {
	title := strings.Join(strings.Fields(titleBody(text)), " ")
	runes := []rune(title)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return title
}
