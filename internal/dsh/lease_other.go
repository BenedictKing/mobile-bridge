//go:build !linux && !darwin && !windows

package dsh

func probeWriteOwner(paths []string) (owned, known bool) {
	return false, false
}
