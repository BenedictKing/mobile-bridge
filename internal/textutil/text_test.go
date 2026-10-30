package textutil

import (
	"strings"
	"testing"
)

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

func TestTitleFromTextStripsProtocolWrappers(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"attributed caveat", `<system-reminder data-role="command-caveat">Caveat: generated locally.</system-reminder>`, ""},
		{"local command", "<command-name>/status</command-name>\n<command-message>status</command-message>\n<command-args></command-args>", ""},
		{"local stdout", "<local-command-stdout>Kept model</local-command-stdout>", ""},
		{"caveat then prompt", `<system-reminder data-role="command-caveat">x</system-reminder>修复列表标题`, "修复列表标题"},
		{"member envelope", "[Team context]\n来源成员：a\n来源任务：lead\n结果\n\n[Assigned task]\n排查失败", "排查失败"},
		{"nested envelope", "[Team context]\n[Assigned task]\n旧任务\n\n[Assigned task]\n新任务", "新任务"},
		{"briefing with header", "[Team delegation]\n你是 Lead\n[Assigned task]\n实现头像上传", "实现头像上传"},
		{"briefing without header", "[Team delegation]\n你是本次自动规划的 Lead", ""},
		{"review briefing", "[Team review]\n只做审查\n[Assigned task]\n审查接口", "审查接口"},
		{"ordinary mention", "修复 [Team context] 显示", "修复 [Team context] 显示"},
	}
	for _, tc := range cases {
		if got := TitleFromText(tc.input); got != tc.want {
			t.Fatalf("%s: title = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := StripInternalReminders(`<system-reminder data-role="x">内部</system-reminder>正文`); got != "正文" {
		t.Fatalf("attributed reminder not stripped: %q", got)
	}
}

func TestCleanTitle(t *testing.T) {
	long := strings.Repeat("长", 90)
	if got := CleanTitle(long); got != long {
		t.Fatalf("ordinary title must stay untouched and untruncated: %d runes", len([]rune(got)))
	}
	if got := CleanTitle("[Team delegation] 你是本次自动规划的 Lead"); got != "" {
		t.Fatalf("protocol-only title = %q", got)
	}
	if got := CleanTitle(`<system-reminder data-role="command-caveat">c</system-reminder>真实标题`); got != "真实标题" {
		t.Fatalf("polluted title = %q", got)
	}
}
