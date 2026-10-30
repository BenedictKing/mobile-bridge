package team

import (
	"errors"
	"fmt"
	"strings"
)

func sessionNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	// zcode-acp-server 的 sessionEvicted（占位映射仍在、后端会话已被清理，zh/en 两种
	// 文案）同属确定的会话缺失；通用的 "Internal error" 不在此列。
	return strings.Contains(text, "session not found") || strings.Contains(text, "session_not_found") ||
		strings.Contains(text, "会话不存在") || strings.Contains(text, "找不到会话") ||
		strings.Contains(text, "已被后端清理") || strings.Contains(text, "no longer exists in the backend")
}

// recoverMissingMemberSession 为已确认丢失的成员会话创建替代会话并回写绑定。
// 未装配恢复端口时保持原行为，由既有派发失败路径归类。
func (s *Service) recoverMissingMemberSession(team *Team, member *TeamMember, cwd string) error {
	recreator, ok := s.resolver.(SessionRecreator)
	if !ok || team == nil || member == nil {
		return nil
	}
	if current := memberOf(s.store.Team(team.ID), member.ID); current != nil && current.SessionID != member.SessionID {
		member.SessionID = current.SessionID
		return nil
	}
	newSessionID, err := recreator.Recreate(member.AgentKind, member.SessionID, cwd)
	if err != nil {
		return fmt.Errorf("会话自动恢复失败：%w", err)
	}
	if newSessionID == "" {
		return errors.New("会话自动恢复失败：Agent 未返回新会话标识")
	}
	for attempt := 0; attempt < 5; attempt++ {
		fresh := s.store.Team(team.ID)
		current := memberOf(fresh, member.ID)
		if current == nil {
			return fmt.Errorf("会话自动恢复失败：成员 %s 已不存在", member.ID)
		}
		if current.SessionID != member.SessionID {
			member.SessionID = current.SessionID
			return nil
		}
		current.SessionID = newSessionID
		if err := s.store.SaveTeam(fresh); err != nil {
			if errors.Is(err, ErrVersionConflict) {
				continue
			}
			return fmt.Errorf("会话自动恢复后回写成员绑定失败：%w", err)
		}
		member.SessionID = newSessionID
		return nil
	}
	return errors.New("会话自动恢复后回写成员绑定失败：连续版本冲突")
}
