package vmm

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// clone copies a disk image as an APFS clone: instant, and sharing blocks
// until either copy changes. A plain copy of a sparse 64 GB file would not
// be, so other file systems aren't supported yet.
func clone(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("snapshots need APFS (cloning %s: %w)", filepath.Base(src), err)
		}
		return err
	}
	return nil
}
