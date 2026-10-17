//go:build !windows

package account

import "syscall"

// sysProcAttrNewSession mirrors Popen(start_new_session=True): the desktop
// App outlives the gateway and owns its own process group.
func sysProcAttrNewSession() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Setsid: true}
}

const syscallSIGTERM = syscall.SIGTERM

// uidOf reads the owner uid from an os.FileInfo's syscall stat (Linux probe).
func uidOf(info any) int {
	if stat, ok := info.(*syscall.Stat_t); ok {
		return int(stat.Uid)
	}
	return -1
}
