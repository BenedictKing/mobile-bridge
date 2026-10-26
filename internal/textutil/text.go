// Package textutil contains display-only text cleanup shared by agent views.
package textutil

import (
	"regexp"
	"strings"
)

var internalReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// StripInternalReminders removes harness-only reminder envelopes from the
// phone timeline. It never changes prompts sent to an agent.
func StripInternalReminders(text string) string {
	cleaned := internalReminder.ReplaceAllString(text, "")
	if cleaned == text {
		return text
	}
	return strings.TrimSpace(cleaned)
}
