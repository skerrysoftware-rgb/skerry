//go:build !darwin

package fsutil

import (
	"os"
	"strconv"
)

// RealPath returns path unchanged: case-insensitive volumes are a macOS
// concern here, and callers already resolve symlinks.
func RealPath(path string) string { return path }

// FDPath is the path the file system gives for an open file.
func FDPath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
