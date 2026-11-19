package httpd

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func migrationHistoryView(entries ...migrationEntry) *jsoncompat.Object {
	view := jsoncompat.NewObject()
	view.Set("title", "设计渠道列表的独立子代理配置标签页")
	view.Set("cwd", "/tmp/ccx-v3-lean")
	messages := &jsoncompat.Array{}
	for _, entry := range entries {
		message := jsoncompat.NewObject()
		message.Set("role", entry.role)
		message.Set("text", entry.text)
		messages.Items = append(messages.Items, message)
	}
	turn := jsoncompat.NewObject()
	turn.Set("messages", messages)
	view.Set("turns", &jsoncompat.Array{Items: []any{turn}})
	return view
}

func TestMigrationHistoryFlattensNestedLegacyTranscripts(t *testing.T) {
	question := "现在子代理模型选择放在渠道编辑里面了？这就不对了。如果设计为在渠道列表页面提供单独的tab切换调整子代理渠道排序和开关呢？"
	answer := "我先核对当前实现，以及子代理渠道开关、排序和普通调度之间的关系，再判断独立 tab 应该控制哪些内容。"
	text := "【从 Codex 迁移的会话】\n标题：设计渠道列表的独立子代理配置标签页\n目录：/tmp/ccx-v3-lean\n\n用户：" + question + "\n助手：" + answer
	for _, label := range []string{"Codex", "Kimi", "Codex"} {
		text = "【从 " + label + " 迁移的会话】\n标题：设计渠道列表的独立子代理配置标签页\n目录：/tmp/ccx-v3-lean\n（转录过长，内容已截断）\n\n用户：" + text
	}
	for _, scope := range []string{"main", "full", "latest"} {
		entries, truncated := migrationTranscript(migrationHistoryView(migrationEntry{"user", text}), scope)
		if len(entries) != 2 || entries[0] != (migrationEntry{"user", question}) || entries[1] != (migrationEntry{"assistant", answer}) || !truncated {
			t.Fatalf("%s lost original exchange: entries=%+v truncated=%v", scope, entries, truncated)
		}
	}
}

func TestMigrationHistoryRepeatedTransfersPreserveDialogueAndInstructions(t *testing.T) {
	question := "在渠道列表提供独立 tab，调整子代理渠道的排序和开关。"
	answer := "关键结论：子代理调度与普通渠道调度分别配置。\n```text\n用户：代码里的角色标签\n助手：保持代码原样\n```\n已完成方案，下一步实现。"
	for _, scope := range []string{"main", "full"} {
		t.Run(scope, func(t *testing.T) {
			view := migrationHistoryView(migrationEntry{"user", question}, migrationEntry{"assistant", answer})
			for round := 0; round < 10; round++ {
				locale, label := "zh-CN", "Codex"
				if round%2 == 1 {
					locale, label = "en-US", "Kimi"
				}
				instructions := fmt.Sprintf("第 %d 次后续指示：保持渠道列表的独立 tab。", round)
				text := migrationMessage(label, view, locale, scope, instructions)
				if len(text) > 100000 || !utf8.ValidString(text) {
					t.Fatalf("round %d invalid wire text: bytes=%d", round, len(text))
				}
				view = migrationHistoryView(migrationEntry{"user", text}, migrationEntry{"assistant", fmt.Sprintf("第 %d 次回复：继续实现。", round)})
				entries, truncated := migrationTranscript(view, scope)
				if truncated || len(entries) != 4+2*round || entries[0] != (migrationEntry{"user", question}) || entries[1] != (migrationEntry{"assistant", answer}) || entries[len(entries)-2] != (migrationEntry{"user", instructions}) {
					t.Fatalf("round %d changed dialogue: entries=%+v truncated=%v", round, entries, truncated)
				}
			}
		})
	}
}

func TestMigrationHistoryLatestUsesRestoredQuestion(t *testing.T) {
	view := migrationHistoryView(
		migrationEntry{"user", "最初的问题"}, migrationEntry{"assistant", "最初的回答"},
		migrationEntry{"user", "更正：排序必须独立"}, migrationEntry{"assistant", "最新关键结论"})
	for _, locale := range []string{"zh-CN", "en-US"} {
		for _, sourceScope := range []string{"main", "latest"} {
			for _, instructions := range []string{"", "继续验证独立排序"} {
				text := migrationMessage("Codex", view, locale, sourceScope, instructions)
				entries, _ := migrationTranscript(migrationHistoryView(migrationEntry{"user", text}, migrationEntry{"assistant", "迁移后的回复"}), "latest")
				if instructions == "" {
					if len(entries) != 3 || entries[0] != (migrationEntry{"user", "更正：排序必须独立"}) || entries[1] != (migrationEntry{"assistant", "最新关键结论"}) || entries[2].text != "迁移后的回复" {
						t.Fatalf("%s/%s latest=%+v", locale, sourceScope, entries)
					}
				} else if len(entries) != 2 || entries[0] != (migrationEntry{"user", instructions}) || entries[1].text != "迁移后的回复" {
					t.Fatalf("%s/%s instructions=%+v", locale, sourceScope, entries)
				}
			}
		}
	}
}

func TestMigrationHistoryLeavesOrdinaryMessagesUntouched(t *testing.T) {
	for _, text := range []string{
		"用户：原始问题\n助手：这是用户写的示例",
		"【从 Codex 迁移的会话】\n标题：缺少目录的普通消息\n用户：仍然是原文",
		"请检查下面的转录：\n【从 Codex 迁移的会话】\n标题：示例\n目录：/tmp/example\n用户：示例问题",
		"```text\n【从 Codex 迁移的会话】\n标题：示例\n目录：/tmp/example\n用户：示例问题\n```",
	} {
		entries, truncated := migrationTranscript(migrationHistoryView(migrationEntry{"user", text}), "main")
		if truncated || len(entries) != 1 || entries[0] != (migrationEntry{"user", text}) {
			t.Fatalf("ordinary message changed: entries=%+v truncated=%v", entries, truncated)
		}
		for _, locale := range []string{"zh-CN", "en-US"} {
			wire := migrationMessage("Codex", migrationHistoryView(migrationEntry{"user", text}), locale, "main", "")
			entries, truncated = migrationTranscript(migrationHistoryView(migrationEntry{"user", wire}), "main")
			if truncated || len(entries) != 1 || entries[0] != (migrationEntry{"user", text}) {
				t.Fatalf("%s round trip changed ordinary message: entries=%+v truncated=%v", locale, entries, truncated)
			}
		}
	}
}

func TestMigrationHistoryPreservesInstructionRoleLabels(t *testing.T) {
	instructions := "按下面的格式展示：\n用户：原始要求\nAssistant: 关键回复\n\n【用户后续指示】\n这是指示正文里的示例。"
	for _, locale := range []string{"zh-CN", "en-US"} {
		text := migrationMessage("Kimi", migrationHistoryView(migrationEntry{"user", "原始问题"}), locale, "main", instructions)
		entries, truncated := migrationTranscript(migrationHistoryView(migrationEntry{"user", text}), "main")
		if truncated || len(entries) != 2 || entries[1] != (migrationEntry{"user", instructions}) {
			t.Fatalf("%s instructions split into messages: entries=%+v truncated=%v", locale, entries, truncated)
		}
	}
}

func TestMigrationHistoryReadsFramedTranscriptInsideLegacyWrapper(t *testing.T) {
	question := "原始问题\n用户：正文中的角色标签"
	inner := migrationMessage("Codex", migrationHistoryView(migrationEntry{"user", question}, migrationEntry{"assistant", "关键回答"}), "en-US", "main", "继续验证")
	outer := "【从 Kimi 迁移的会话】\n标题：标题\n目录：/tmp/project\n\n用户：" + inner + "\n助手：外层回复"
	entries, truncated := migrationTranscript(migrationHistoryView(migrationEntry{"user", outer}), "main")
	if truncated || len(entries) != 4 || entries[0] != (migrationEntry{"user", question}) || entries[1] != (migrationEntry{"assistant", "关键回答"}) || entries[2] != (migrationEntry{"user", "继续验证"}) || entries[3] != (migrationEntry{"assistant", "外层回复"}) {
		t.Fatalf("mixed formats changed dialogue: entries=%+v truncated=%v", entries, truncated)
	}
}

func TestMigrationHistoryDamagedFramePreservesSourceText(t *testing.T) {
	question := "用户：正文中的角色标签\nAssistant: 正文中的回复示例"
	text := migrationMessage("Codex", migrationHistoryView(migrationEntry{"user", question}), "zh-CN", "main", "")
	text = strings.Replace(text, fmt.Sprintf("user:%d", len(question)), fmt.Sprintf("user:%d", len(question)+1), 1)
	entries, truncated := migrationTranscript(migrationHistoryView(migrationEntry{"user", text}), "main")
	if truncated || len(entries) != 1 || entries[0] != (migrationEntry{"user", text}) {
		t.Fatalf("damaged frame discarded or split text: entries=%+v truncated=%v", entries, truncated)
	}
}

func TestMigrationHistoryBudgetKeepsOriginalTaskAndFinalReply(t *testing.T) {
	question := "ORIGINAL_TASK：在渠道列表页面独立配置子代理渠道排序和开关。"
	view := migrationHistoryView(migrationEntry{"user", question}, migrationEntry{"assistant", "KEY_REPLY：配置应与普通调度分离。"})
	text := migrationMessage("Codex", view, "zh-CN", "full", "")
	text = "【从 Kimi 迁移的会话】\n标题：标题\n目录：/tmp/project\n\n用户：" + text + "\n助手：" + strings.Repeat("冗长进度说明。", 15000)
	view = migrationHistoryView(migrationEntry{"user", text}, migrationEntry{"user", "LATEST_QUESTION：继续完成验证。"}, migrationEntry{"assistant", "FINAL_ANSWER：" + strings.Repeat("结论细节。", 15000) + "END_OF_RESULT"})
	for _, scope := range []string{"main", "full"} {
		message := migrationMessage("Codex", view, "zh-CN", scope, strings.Repeat("后续指示。", 1500))
		if len(message) > 100000 || !utf8.ValidString(message) {
			t.Fatalf("%s invalid wire text: bytes=%d", scope, len(message))
		}
		for _, needle := range []string{question, "KEY_REPLY", "LATEST_QUESTION", "FINAL_ANSWER", "END_OF_RESULT", "内容已截断"} {
			if !strings.Contains(message, needle) {
				t.Fatalf("%s lost %q", scope, needle)
			}
		}
	}
}
