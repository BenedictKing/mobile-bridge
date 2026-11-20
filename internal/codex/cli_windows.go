package codex

import (
	"errors"
	"io"
)

func connectCLITransport(executable, home, path string) (io.ReadWriteCloser, error) {
	return nil, errors.New("当前 Codex CLI 接管需要 Unix socket；Windows 请使用桌面 IPC")
}
