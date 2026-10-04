//go:build !unix

package main

// denmaDiskUsage isn't available here (System, cmd/denma_system.go).
func denmaDiskUsage(p string) (total, free uint64, ok bool) {
	return 0, 0, false
}
