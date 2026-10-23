//go:build !darwin

package fsutil

// RealPath returns path unchanged: case-insensitive volumes are a macOS
// concern here, and callers already resolve symlinks.
func RealPath(path string) string { return path }
