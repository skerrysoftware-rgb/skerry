package vmm

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"

	"github.com/skerrysoftware-rgb/skerry/internal/fsevents"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

// Directories whose changes aren't forwarded: git's own, and the output
// and dependency trees that tools in the VM write themselves (forwarding
// those would only echo the VM's own writes back to it).
var quietDirs = map[string]bool{
	".git": true, "node_modules": true, ".next": true, ".turbo": true, ".cache": true,
	"dist": true, "build": true, "coverage": true, "__pycache__": true, ".venv": true,
}

// maxTouchBatch and maxTouchBytes bound one batch of forwarded file
// events; the agent refuses batches over a megabyte.
const (
	maxTouchBatch = 2000
	maxTouchBytes = 512 << 10
)

// forwardFileEvents passes changes made to the checkout on the host to the
// guest, so file watchers there (dev servers, test runners) see them. It
// can be turned off with SKERRY_FILE_EVENTS=off.
func forwardFileEvents(ws registry.Workspace, d *guestDialer) (*fsevents.Watcher, error) {
	if os.Getenv("SKERRY_FILE_EVENTS") == "off" {
		return nil, nil
	}
	// FSEvents reports a path again when the guest re-stamps it (its flags
	// accumulate), so a path is forwarded only when what a watcher cares
	// about changed: contents (size, modification time) or existence. A
	// state is remembered only once the agent has it, so a failed send is
	// retried at the next event. Keys are hashes, so a guest making files
	// with very long names can't make the map large. The callback runs on
	// one goroutine at a time.
	seen := map[[16]byte]fileState{}
	return fsevents.Watch(ws.Path, 0.05, func(paths []string) {
		type change struct {
			rel   string
			key   [16]byte
			state fileState
		}
		var batch []change
		size := 0
		send := func() {
			if len(batch) == 0 {
				return
			}
			rels := make([]string, len(batch))
			for i, c := range batch {
				rels[i] = c.rel
			}
			c, err := d.client()
			if err == nil {
				if err = c.Touch(rels); err != nil {
					d.drop(c)
				}
			}
			if err == nil {
				if len(seen)+len(batch) > maxTracked {
					clear(seen)
				}
				for _, c := range batch {
					seen[c.key] = c.state
				}
			}
			batch, size = batch[:0], 0
		}
		for _, p := range paths {
			rel, err := filepath.Rel(ws.Path, p)
			if err != nil || !filepath.IsLocal(rel) || quiet(rel) {
				continue
			}
			key, st := pathKey(p), stateOf(p)
			if prev, ok := seen[key]; ok && prev == st {
				continue
			}
			batch = append(batch, change{rel, key, st})
			if size += len(rel) + 1; len(batch) == maxTouchBatch || size >= maxTouchBytes {
				send() // big changes (a checkout) go in several requests
			}
		}
		send()
	})
}

// maxTracked bounds the file states remembered between events.
const maxTracked = 100000

func pathKey(p string) [16]byte {
	sum := sha256.Sum256([]byte(p))
	return [16]byte(sum[:16])
}

type fileState struct {
	exists      bool
	size, mtime int64
}

func stateOf(path string) fileState {
	info, err := os.Lstat(path)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, size: info.Size(), mtime: info.ModTime().UnixNano()}
}

// quiet reports whether rel is inside one of quietDirs. Only directories
// count: a file named build is forwarded.
func quiet(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		if quietDirs[part] {
			return true
		}
	}
	return false
}
