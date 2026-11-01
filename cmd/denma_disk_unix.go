//go:build unix

package main

import "syscall"

// denmaDiskUsage returns the total and free space of the disk a path is on.
func denmaDiskUsage(p string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0, false
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), true
}
