// Package engine runs workspace VMs. Each platform has its own driver; this
// file holds what they share.
package engine

import (
	"errors"
	"io"
)

// Config describes one VM.
type Config struct {
	// Kernel is an uncompressed kernel Image; Initramfs carries the agent.
	Kernel, Initramfs string
	// Rootfs is the image directory, shared read-only unless RootfsWritable.
	Rootfs         string
	RootfsWritable bool
	// ConfigDir is shared read-only with the agent's config and keys.
	ConfigDir string
	// WorkDir is the checkout, shared read-write. Empty shares nothing.
	WorkDir string
	CPUs    uint
	// MemoryMiB caps the guest's memory.
	MemoryMiB uint64
	// MachineIDFile persists the VM's identity, which restoring a saved VM
	// requires. It is created on first use.
	MachineIDFile string
	// Console receives the guest's console output.
	Console io.Writer
}

// ErrUnsupported is returned on platforms without a driver.
var ErrUnsupported = errors.New("skerry cannot run VMs on this platform yet")
