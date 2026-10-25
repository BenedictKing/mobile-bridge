package httpd

// teams_adapter_test 验证成员间转发（§7）的会话转录适配：teamTranscriptText
// 只输出「角色：文本」正文行——没有 migrationMessage 的 migrate 专属头/
// 标题/目录行，也没有「继续」指令尾行（套进 [Team context] 会形成双层信封）；
// scope 预算与截断标记复用 migrationTranscript。

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestTeamTranscriptTextHasNoMigrateEnvelope(t *testing.T) {
	view := transcriptView()
	text, truncated, err := teamTranscriptText(view, "main")
	if err != nil {
		t.Fatalf("转录组装失败: %v", err)
	}
	if truncated {
		t.Fatalf("小视图不应截断: %q", text)
	}
	// 不得出现 migrationMessage 的任何信封成分。
	for _, marker := range []string{
		"【从", "迁移的会话】", "[Session migrated", "标题：", "目录：",
		"请在此基础上继续", "Please continue from this context", "继续。",
	} {
		if strings.Contains(text, marker) {
			t.Fatalf("转录正文不应包含 migrate 信封成分 %q:\n%s", marker, text)
		}
	}
	// 正文是逐条「角色：文本」行（zh-CN roles 表）。
	for _, want := range []string{
		"用户：第一个问题",
		"助手：第一个回答",
		"操作：执行命令：重要操作的第一行",
		"错误：出错了",
		"用户：第二个问题",
		"助手：第二个回答",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("转录正文应包含 %q:\n%s", want, text)
		}
	}
	// latest scope：只保留最后一问一答。
	latest, _, _ := teamTranscriptText(view, "latest")
	if strings.Contains(latest, "第一个问题") || !strings.Contains(latest, "第二个回答") {
		t.Fatalf("latest 应只保留最后一问一答:\n%s", latest)
	}
	// 空视图：无正文、无截断。
	if text, truncated, _ := teamTranscriptText(jsoncompat.NewObject(), "main"); text != "" || truncated {
		t.Fatalf("空视图应产出空正文，实际 %q truncated=%v", text, truncated)
	}
}

func TestTeamTranscriptTextPropagatesTruncation(t *testing.T) {
	view := transcriptView()
	original := migrationBudgets["main"]
	migrationBudgets["main"] = [3]int{2, 100000, 6000}
	defer func() { migrationBudgets["main"] = original }()
	text, truncated, _ := teamTranscriptText(view, "main")
	if !truncated {
		t.Fatalf("预算收紧应传导截断标记: %q", text)
	}
	if !strings.Contains(text, "第二个问题") {
		t.Fatalf("尾部偏置应保留最新内容:\n%s", text)
	}
}

func TestTeamTranscriptSourceRejectsUnknownScope(t *testing.T) {
	source := NewTeamTranscriptSource(&Server{})
	if _, _, err := source.BoundedTranscript("sess-1", "bogus"); err == nil ||
		!strings.Contains(err.Error(), "未知的转录范围") {
		t.Fatalf("未知 scope 应报错，实际 %v", err)
	}
}
