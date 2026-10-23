package fsutil

import (
	"os"
	"sync"
)

// CappedLog is a log file that never grows past Max bytes: when it would,
// the file is renamed to name.1 (replacing any older one) and a new file is
// started. Output a VM writes can therefore never fill the host's disk.
type CappedLog struct {
	path string
	max  int64
	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenCappedLog starts a fresh log at path, keeping at most max bytes in it
// and max more in path.1.
func OpenCappedLog(path string, max int64) (*CappedLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &CappedLog{path: path, max: max, f: f}, nil
}

// Write always reports all of p written, even when it kept only the end of
// an oversized write: writers such as io.MultiWriter treat a short count
// as an error.
func (l *CappedLog) Write(p []byte) (int, error) {
	whole := len(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > l.max {
		l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return 0, err
		}
		l.f, l.size = f, 0
		if int64(len(p)) > l.max {
			p = p[int64(len(p))-l.max:] // keep only the newest part
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	if err != nil {
		return n, err
	}
	return whole, nil
}

// Close closes the current file.
func (l *CappedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
