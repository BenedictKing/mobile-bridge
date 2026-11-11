package claude

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestTranscriptImagesBecomeAttachmentsWithoutChangingSource(t *testing.T) {
	root, media := t.TempDir(), t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\nclaude-image")
	encoded := base64.StdEncoding.EncodeToString(png)
	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}}`
	content := `{"type":"user","cwd":"/work","message":{"content":[{"type":"text","text":"看图"},IMAGE]}}
{"type":"user","cwd":"/work","message":{"content":[IMAGE]}}
{"type":"assistant","message":{"content":[IMAGE,{"type":"tool_use","id":"shot","name":"screenshot","input":{}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"shot","content":[{"type":"text","text":"截图结果"},IMAGE]}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"完成"}]}}
`
	content = strings.ReplaceAll(content, "IMAGE", image)
	path := filepath.Join(project, "image-session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := TranscriptReader(root, media)("image-session")
	if err != nil || snapshot == nil || len(snapshot.Turns) != 2 {
		t.Fatalf("图片历史读取失败: %v %v", snapshot, err)
	}
	count := 0
	for _, turn := range snapshot.Turns {
		if strings.Contains(string(jsoncompat.EncodeCompact(turn)), encoded) {
			t.Fatal("历史视图仍包含 Base64")
		}
		for _, item := range turn.MustGet("messages").(*jsoncompat.Array).Items {
			row := item.(*jsoncompat.Object)
			attachments, _ := row.MustGet("attachments").(*jsoncompat.Array)
			if attachments == nil {
				continue
			}
			for _, value := range attachments.Items {
				attachment := value.(*jsoncompat.Object)
				file, _ := attachment.MustGet("path").(string)
				if raw, err := os.ReadFile(file); err != nil || string(raw) != string(png) || attachment.MustGet("type") != "localImage" {
					t.Fatalf("图片附件字节或类型错误: %v", err)
				}
				count++
			}
		}
	}
	if count != 4 {
		t.Fatalf("用户、纯图片、助手和工具应各保留一个附件，实际 %d", count)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != content {
		t.Fatal("读取缩略图不能改写 Claude 原始历史")
	}
	if err := os.WriteFile(filepath.Join(project, "image-only.jsonl"), []byte(`{"type":"user","cwd":"/work","message":{"content":[`+image+`]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if rows := ListSessions(root); len(rows) != 2 {
		t.Fatalf("未配置媒体目录的列表扫描也不能丢失纯图片会话: %d", len(rows))
	}
}
