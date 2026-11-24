package httpd

import (
	"errors"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/ipc"
)

// A connection failure never changes this conversation's execution target.
func (s *Server) watchCodexForWrite(threadID string) (*codex.Session, error) {
	session, err := s.Codex.WatchForWrite(threadID, 1500*time.Millisecond)
	if errors.Is(err, ipc.ErrUnavailable) {
		return session, errors.New("桌面 App 连接不可用；请恢复桌面连接，或在聊天详情中显式交给网关执行")
	}
	if err == nil && !s.Codex.IsCLI(threadID) && (session == nil || s.Codex.Owner(session) == "") {
		return session, errors.New("桌面 App 尚未接管此聊天，请在桌面打开后重新连接")
	}
	return session, err
}
