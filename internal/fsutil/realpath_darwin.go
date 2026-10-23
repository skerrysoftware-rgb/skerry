package fsutil

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// RealPath returns path as the filesystem stores it: symlinks resolved and,
// on a case-insensitive volume, every component in its on-disk case. It
// returns path unchanged if it can't be opened.
// FDPath is the path the file system gives for an open file.
func FDPath(f *os.File) (string, error) {
	buf := make([]byte, unix.PathMax)
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return "", errors.New("path too long")
}

func RealPath(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return path
	}
	defer f.Close()
	buf := make([]byte, unix.PathMax)
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return path
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return path
}
