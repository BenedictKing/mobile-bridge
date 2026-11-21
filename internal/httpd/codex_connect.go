package httpd

import (
	"errors"
	"runtime"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/ipc"
)

// watchCodexForWrite may select CLI execution for an explicit send/reconnect.
// History readers keep using Watch and never acquire a new execution owner.
func (s *Server) watchCodexForWrite(threadID string) (*codex.Session, error) {
	session, err := s.Codex.WatchForWrite(threadID, 1500*time.Millisecond)
	if err == nil || s.Codex.IsCLI(threadID) || !errors.Is(err, ipc.ErrUnavailable) || runtime.GOOS == "windows" {
		return session, err
	}
	if s.Creations == nil {
		return session, err
	}
	if s.hiddenSession(threadID) {
		return nil, errors.New("此聊天已删除")
	}
	if requestID, pending := s.Creations.pendingForTarget(threadID); pending != nil {
		if err := s.useCodexCLI(requestID, threadID, pending.ProjectCwd); err != nil {
			return nil, err
		}
	} else {
		if s.Store == nil {
			return session, err
		}
		limit := 1
		history, err := s.Store.HistoryFor(threadID, &limit)
		if err != nil {
			return nil, err
		}
		cwd := stringField(history, "cwd")
		if cwd == "" {
			return nil, errors.New("找不到此 Codex 聊天的工作目录，未接管")
		}
		// Persist before attaching or sending; a failed write must never leave
		// an unrecorded CLI owner that disappears after gateway restart.
		entry, _, err := s.Creations.begin("codex-cli:"+threadID, &creationEntry{
			Agent: "codex", ID: threadID, Host: "local", ProjectCwd: cwd,
			Title: stringField(history, "title"), Execution: "cli",
			At: float64(time.Now().UnixMilli()) / 1000,
		})
		if err != nil {
			return nil, err
		}
		if entry.ID != threadID || entry.ProjectCwd != cwd || entry.Execution != "cli" {
			return nil, errors.New("Codex CLI 执行账本与此聊天不匹配，未接管")
		}
		s.Codex.RegisterCLI(threadID, cwd)
	}
	return s.Codex.WatchForWrite(threadID, 1500*time.Millisecond)
}
