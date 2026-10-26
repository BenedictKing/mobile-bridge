package textutil

import "testing"

func TestStripInternalReminders(t *testing.T) {
	input := `<system-reminder>
The todo list has not been updated. Do not show this to the user.
</system-reminder>

<system-reminder>
Another internal note.
</system-reminder>
实际回复 **保留**。`
	if got := StripInternalReminders(input); got != "实际回复 **保留**。" {
		t.Fatalf("filtered text = %q", got)
	}
	plain := "普通 <system-reminder> 文本与 <other>XML</other>"
	if got := StripInternalReminders(plain); got != plain {
		t.Fatalf("ordinary text changed: %q", got)
	}
}
