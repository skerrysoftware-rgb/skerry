//go:build !darwin

package fsutil

// SetImmutable does nothing yet off macOS: Linux's immutable attribute needs
// root. Linux hosts come with their own driver.
func SetImmutable(string, bool) error { return nil }

// IsImmutable is always false off macOS.
func IsImmutable(string) bool { return false }
