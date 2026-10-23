package vmm

import "golang.org/x/sys/unix"

// memTotal is the machine's memory in bytes, or 0 if unknown.
func memTotal() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
}
