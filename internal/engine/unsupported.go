//go:build !darwin

package engine

import "net"

// VM is unavailable on this platform.
type VM struct{}

// Start reports that this platform has no driver yet.
func Start(Config) (*VM, error) { return nil, ErrUnsupported }

// Done never closes.
func (v *VM) Done() <-chan struct{} { return nil }

// Err is always nil.
func (v *VM) Err() error { return nil }

// Dial is unavailable.
func (v *VM) Dial(uint32) (net.Conn, error) { return nil, ErrUnsupported }

// Stop does nothing.
func (v *VM) Stop() error { return nil }
