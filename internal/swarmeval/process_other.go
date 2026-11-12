//go:build !unix

package swarmeval

import "os/exec"

func ProcessTreeSupported() bool { return false }

func cleanupProcess(cmd *exec.Cmd) {}

// 此平台尚未提供进程树终止能力，启动前 fail-closed，而非冒充完整隔离。
func configureProcess(cmd *exec.Cmd) { cmd.Path = "" }
