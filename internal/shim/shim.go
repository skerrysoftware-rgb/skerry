// Package shim manages the links that route commands such as node and yarn
// into a workspace VM. Each link points at the skerry binary, which looks at
// the name it was run as.
package shim

import (
	"os"
	"path/filepath"

	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/services"
)

// Names are the commands that can run inside the workspace VM: every
// runtime's commands. Node's always do; another runtime's do only in
// workspaces whose project has that runtime.
var Names = services.AllRuntimeCommands()

// Is reports whether skerry was invoked through a shim link.
func Is(argv0 string) bool {
	base := filepath.Base(argv0)
	for _, n := range Names {
		if base == n {
			return true
		}
	}
	return false
}

// Dir is where the shim links live.
func Dir() (string, error) {
	dirs, err := paths.Default()
	if err != nil {
		return "", err
	}
	return filepath.Join(dirs.State, "shims"), nil
}

// Ensure points every shim link at the running skerry executable and
// returns the shim directory.
func Ensure() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, n := range Names {
		link := filepath.Join(dir, n)
		if target, err := os.Readlink(link); err == nil && target == self {
			continue
		}
		_ = os.Remove(link)
		if err := os.Symlink(self, link); err != nil {
			return "", err
		}
	}
	return dir, nil
}
