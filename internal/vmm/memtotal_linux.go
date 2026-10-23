//go:build linux

package vmm

import "golang.org/x/sys/unix"

// memTotal is the machine's memory in bytes, or 0 if unknown.
func memTotal() uint64 {
	var info unix.Sysinfo_t
	if unix.Sysinfo(&info) != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}
