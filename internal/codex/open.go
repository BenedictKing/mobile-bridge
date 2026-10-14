package codex

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"

	"github.com/google/uuid"
)

// OpenInDesktop hands the freshly created thread to the desktop App through
// its codex:// URL scheme, mirroring bridge/create.py's open_in_desktop: the
// App becomes the execution owner so the phone can attach.
func OpenInDesktop(threadID string) error {
	if _, err := uuid.Parse(threadID); err != nil {
		return errors.New("会话标识无效")
	}
	url := "codex://threads/" + threadID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", url)
	case "linux":
		command = exec.CommandContext(ctx, "xdg-open", url)
	case "windows":
		command = exec.CommandContext(ctx, "cmd", "/c", "start", "", url)
	default:
		return errors.New("请在支持 Codex App 的 Mac 或 Windows 电脑运行网关")
	}
	return command.Run()
}
