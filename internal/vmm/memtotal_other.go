//go:build !darwin && !linux

package vmm

// memTotal is unknown here.
func memTotal() uint64 { return 0 }
