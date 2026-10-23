// Package scope records the folders where skerry is enabled.
//
// skerry is opt-in. Outside enabled folders its Claude Code hook creates
// plain worktrees, the way Claude Code does by default, and runs nothing
// else. Enabling a folder also means trusting it: skerry will run the
// projects' setup, migration and service commands, inside workspace VMs.
package scope

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

// Entry is one enabled folder. Everything below it is enabled.
type Entry struct {
	Dir       string    `json:"dir"`
	EnabledAt time.Time `json:"enabled_at"`
}

type file struct {
	Version int     `json:"version"`
	Enabled []Entry `json:"enabled"`
	// Hinted lists repositories where Claude has already been told, once,
	// that skerry is available but off.
	Hinted []string `json:"hinted,omitempty"`
}

const fileVersion = 1

// Store is the scope list, kept as a JSON file next to the registry.
type Store struct {
	path string
	now  func() time.Time
}

// Open returns the scope store in dir.
func Open(dir string) *Store {
	return &Store{path: filepath.Join(dir, "scope.json"), now: time.Now}
}

// IsEnabled reports whether path is inside an enabled folder. path must be
// absolute and symlink-free.
func (s *Store) IsEnabled(path string) (bool, error) {
	var enabled bool
	err := s.update(false, func(f *file) error {
		for _, e := range f.Enabled {
			if within(path, e.Dir) {
				enabled = true
			}
		}
		return nil
	})
	return enabled, err
}

// Enable adds dir. Enabling a folder that is already covered is harmless.
func (s *Store) Enable(dir string) error {
	return s.update(true, func(f *file) error {
		for _, e := range f.Enabled {
			if e.Dir == dir {
				return nil
			}
		}
		f.Enabled = append(f.Enabled, Entry{Dir: dir, EnabledAt: s.now().UTC()})
		return nil
	})
}

// Disable removes dir and reports whether it was enabled. A folder enabled
// through a parent stays enabled; Covering returns that parent.
func (s *Store) Disable(dir string) (bool, error) {
	var removed bool
	err := s.update(true, func(f *file) error {
		kept := f.Enabled[:0]
		for _, e := range f.Enabled {
			if e.Dir == dir {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		f.Enabled = kept
		return nil
	})
	return removed, err
}

// Covering returns the enabled folder that contains path, if any.
func (s *Store) Covering(path string) (string, error) {
	var dir string
	err := s.update(false, func(f *file) error {
		for _, e := range f.Enabled {
			if within(path, e.Dir) && len(e.Dir) > len(dir) {
				dir = e.Dir
			}
		}
		return nil
	})
	return dir, err
}

// List returns enabled folders sorted by path.
func (s *Store) List() ([]Entry, error) {
	var out []Entry
	err := s.update(false, func(f *file) error {
		out = append(out, f.Enabled...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, err
}

// FirstHint records that repo has been hinted about and reports whether
// this was the first time.
func (s *Store) FirstHint(repo string) (bool, error) {
	first := false
	err := s.update(true, func(f *file) error {
		for _, r := range f.Hinted {
			if r == repo {
				return nil
			}
		}
		first = true
		f.Hinted = append(f.Hinted, repo)
		return nil
	})
	return first, err
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func (s *Store) update(write bool, fn func(*file) error) error {
	unlock, err := fsutil.Lock(s.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	f := &file{Version: fileVersion}
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read scope: %w", err)
	default:
		if err := json.Unmarshal(data, f); err != nil {
			return fmt.Errorf("scope file %s is corrupt: %w", s.path, err)
		}
		if f.Version != fileVersion {
			return fmt.Errorf("scope file %s has version %d; this skerry understands %d", s.path, f.Version, fileVersion)
		}
	}
	if err := fn(f); err != nil || !write {
		return err
	}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, append(out, '\n'), 0o600)
}
