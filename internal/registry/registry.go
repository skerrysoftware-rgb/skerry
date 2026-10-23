// Package registry records the workspaces skerry has created.
//
// The registry is the only source skerry trusts for which paths it may
// delete: a path is removed only if it is recorded here.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
)

// Workspace is one registered checkout.
type Workspace struct {
	// ID is a random identifier, stable for the workspace's lifetime.
	ID string `json:"id"`
	// Name is unique within a repository.
	Name string `json:"name"`
	// Repo is the main checkout's root directory.
	Repo string `json:"repo"`
	// Path is the worktree directory.
	Path string `json:"path"`
	// Branch is the branch skerry created for the worktree.
	Branch string `json:"branch"`
	// Base is the commit the branch started from.
	Base string `json:"base"`
	// Mode is ModeSkerry for a full environment, or ModePlain for a plain
	// worktree created where skerry isn't enabled.
	Mode string `json:"mode"`
	// CreatedBy says what asked for the workspace, e.g. "cli" or "claude".
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Workspace modes.
const (
	ModeSkerry = "skerry"
	ModePlain  = "plain"
)

// ErrNotFound is returned when no workspace matches.
var ErrNotFound = errors.New("workspace not found")

type file struct {
	Version    int         `json:"version"`
	Workspaces []Workspace `json:"workspaces"`
}

const fileVersion = 1

// Registry is a JSON file guarded by an advisory lock, so concurrent skerry
// processes (several Claude sessions starting at once) don't lose updates.
type Registry struct {
	path string
}

// Open returns the registry stored in dir.
func Open(dir string) *Registry {
	return &Registry{path: filepath.Join(dir, "workspaces.json")}
}

// List returns all workspaces, sorted by repository then name.
func (r *Registry) List() ([]Workspace, error) {
	var out []Workspace
	err := r.withLock(false, func(f *file) error {
		out = append(out, f.Workspaces...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Name < out[j].Name
	})
	return out, err
}

// Add records a workspace. It fails if the name is taken in the same
// repository or the path is already registered.
func (r *Registry) Add(ws Workspace) error {
	return r.withLock(true, func(f *file) error {
		for _, existing := range f.Workspaces {
			if existing.Repo == ws.Repo && existing.Name == ws.Name {
				return fmt.Errorf("workspace %q already exists in %s", ws.Name, ws.Repo)
			}
			if existing.Path == ws.Path {
				return fmt.Errorf("path %s is already registered as %q", ws.Path, existing.Name)
			}
		}
		f.Workspaces = append(f.Workspaces, ws)
		return nil
	})
}

// Remove deletes the workspace with the given ID.
func (r *Registry) Remove(id string) error {
	return r.withLock(true, func(f *file) error {
		for i, ws := range f.Workspaces {
			if ws.ID == id {
				f.Workspaces = append(f.Workspaces[:i], f.Workspaces[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

// FindByPath returns the workspace whose directory contains path. path must
// already be absolute and symlink-free.
func (r *Registry) FindByPath(path string) (Workspace, error) {
	var found Workspace
	err := r.withLock(false, func(f *file) error {
		best := -1
		for i, ws := range f.Workspaces {
			if within(path, ws.Path) && (best < 0 || len(ws.Path) > len(f.Workspaces[best].Path)) {
				best = i
			}
		}
		if best < 0 {
			return ErrNotFound
		}
		found = f.Workspaces[best]
		return nil
	})
	return found, err
}

// FindByName returns the workspace with this name. When repo is empty and
// several repositories use the name, it reports the ambiguity.
func (r *Registry) FindByName(repo, name string) (Workspace, error) {
	var matches []Workspace
	err := r.withLock(false, func(f *file) error {
		for _, ws := range f.Workspaces {
			if ws.Name == name && (repo == "" || ws.Repo == repo) {
				matches = append(matches, ws)
			}
		}
		return nil
	})
	if err != nil {
		return Workspace{}, err
	}
	switch len(matches) {
	case 0:
		return Workspace{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		repos := make([]string, len(matches))
		for i, m := range matches {
			repos[i] = m.Repo
		}
		return Workspace{}, fmt.Errorf("workspace %q exists in several repositories (%s); run skerry from inside one", name, strings.Join(repos, ", "))
	}
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// withLock runs fn on the registry's contents under the lock, and writes
// them back atomically when write is true and fn succeeds.
func (r *Registry) withLock(write bool, fn func(*file) error) error {
	unlock, err := fsutil.Lock(r.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	f, err := r.read()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	if !write {
		return nil
	}
	return r.write(f)
}

func (r *Registry) read() (*file, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return &file{Version: fileVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("registry %s is corrupt: %w", r.path, err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("registry %s has version %d; this skerry understands %d", r.path, f.Version, fileVersion)
	}
	return &f, nil
}

func (r *Registry) write(f *file) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(r.path, append(data, '\n'), 0o600)
}
