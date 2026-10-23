//go:build !darwin

package fsevents

import "errors"

// Watcher reports changed paths under a directory.
type Watcher struct{}

// Watch is only implemented on macOS; on Linux, virtio-fs and inotify
// will need their own answer.
func Watch(dir string, latency float64, fn func(paths []string)) (*Watcher, error) {
	return nil, errors.New("file events are not supported on this platform yet")
}

// Close stops watching.
func (w *Watcher) Close() {}
