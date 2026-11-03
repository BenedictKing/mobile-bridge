//go:build !linux && !darwin

package dsh

func probeWriteOwner(paths []string) (owned, known bool) {
	return false, false
}
