//go:build windows

package account

import "syscall"

// sysProcAttrNewSession mirrors Popen(creationflags=CREATE_NEW_PROCESS_GROUP):
// the desktop App is its own Ctrl+C group and never inherits the console.
func sysProcAttrNewSession() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}

const syscallSIGTERM = syscall.SIGTERM

// uidOf is unused on Windows (the /proc probe is Linux-only).
func uidOf(info any) int { return -1 }
