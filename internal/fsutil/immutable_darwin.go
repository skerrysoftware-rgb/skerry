package fsutil

import (
	"os"
	"syscall"
)

// ufImmutable is the user-immutable flag (chflags uchg). An immutable file
// can't be written, renamed or deleted until the flag is cleared, and a VM's
// shared-folder protocol has no way to clear it.
const ufImmutable = 0x2

// SetImmutable sets or clears the user-immutable flag on path itself
// (symlinks are not followed).
func SetImmutable(path string, on bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	flags := 0
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		flags = int(st.Flags)
	}
	if on {
		flags |= ufImmutable
	} else {
		flags &^= ufImmutable
	}
	return syscall.Chflags(path, flags)
}

// IsImmutable reports whether path has the user-immutable flag.
func IsImmutable(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&ufImmutable != 0
}
