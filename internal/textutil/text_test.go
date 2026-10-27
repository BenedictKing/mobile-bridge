package textutil

import "testing"

func TestTitleFromText(t *testing.T) {
	input := "<system-reminder>内部</system-reminder>\n  修复  Kimi\t标题  "
	if got := TitleFromText(input); got != "修复 Kimi 标题" {
		t.Fatalf("title = %q", got)
	}
	if got := TitleFromText("<system-reminder>只有内部内容</system-reminder>"); got != "" {
		t.Fatalf("internal-only title = %q", got)
	}
	long := "123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"
	if got := []rune(TitleFromText(long)); len(got) != 81 || got[80] != '…' {
		t.Fatalf("long title = %q (%d runes)", string(got), len(got))
	}
}

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
