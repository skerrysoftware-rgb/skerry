//go:build !darwin

package vmm

import "errors"

// clone needs a copy-on-write clone (reflink on Linux); not yet.
func clone(src, dst string) error {
	return errors.New("snapshots are not supported on this platform yet")
}
