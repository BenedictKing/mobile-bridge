package model

import (
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// approvalReviewText 在时间线截断前生成正文，避免 180 字预览只剩内部 ID 和半段 JSON。
func approvalReviewText(item *jsoncompat.Object) string {
	review := obj(item.MustGet("review"))
	read := func(keys ...string) string {
		for _, source := range []*jsoncompat.Object{item, review} {
			if source == nil {
				continue
			}
			for _, key := range keys {
				if value, ok := source.MustGet(key).(string); ok && strings.TrimSpace(value) != "" {
					return value
				}
			}
		}
		return ""
	}
	var lines []string
	appendField := func(label, value string) {
		if value != "" {
			lines = append(lines, label+"："+value)
		}
	}
	status := read("status", "decision")
	statusLabels := map[string]string{"approved": "已批准", "allow": "已批准", "denied": "已拒绝", "rejected": "已拒绝", "deny": "已拒绝", "inProgress": "审查中", "pending": "审查中", "completed": "审查已结束", "failed": "审查失败", "interrupted": "已中断", "cancelled": "已取消"}
	if label := statusLabels[status]; label != "" {
		status = label
	}
	appendField("审批状态", status)
	levels := map[string]string{"low": "低", "medium": "中", "high": "高", "unknown": "未知"}
	levelText := func(value string) string {
		if label := levels[value]; label != "" {
			return label
		}
		return value
	}
	appendField("风险等级", levelText(read("riskLevel", "risk")))
	appendField("用户授权", levelText(read("userAuthorization")))
	appendField("审查依据", read("rationale", "reason", "summary"))
	if action := obj(item.MustGet("action")); action != nil {
		if command, ok := action.MustGet("command").(string); ok {
			appendField("命令", command)
		}
		if cwd, ok := action.MustGet("cwd").(string); ok {
			appendField("工作目录", cwd)
		}
	}
	if len(lines) == 0 {
		return "审批审查已记录"
	}
	return strings.Join(lines, "\n")
}
