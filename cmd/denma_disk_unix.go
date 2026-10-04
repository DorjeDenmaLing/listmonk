//go:build unix

package main

import "syscall"

// denmaDiskUsage is the size of the disk a path is on, and its space free to
// the app (System, cmd/denma_system.go).
func denmaDiskUsage(p string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0, false
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), true
}
