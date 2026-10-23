// Package trust records which repositories the user allows skerry to run
// commands for. A repository is untrusted until the user says otherwise:
// cloning it must never be enough to run its code.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
)

// Entry is one trusted repository.
type Entry struct {
	// Root is the main checkout's directory.
	Root      string    `json:"root"`
	TrustedAt time.Time `json:"trusted_at"`
}

type file struct {
	Version int     `json:"version"`
	Repos   []Entry `json:"repos"`
}

const fileVersion = 1

// Store is the trust list, kept as a JSON file next to the registry.
type Store struct {
	path string
	now  func() time.Time
}

// Open returns the trust store in dir.
func Open(dir string) *Store {
	return &Store{path: filepath.Join(dir, "trust.json"), now: time.Now}
}

// IsTrusted reports whether root is trusted.
func (s *Store) IsTrusted(root string) (bool, error) {
	var trusted bool
	err := s.update(false, func(f *file) error {
		for _, e := range f.Repos {
			if e.Root == root {
				trusted = true
			}
		}
		return nil
	})
	return trusted, err
}

// Trust marks root as trusted. Trusting twice is harmless.
func (s *Store) Trust(root string) error {
	return s.update(true, func(f *file) error {
		for _, e := range f.Repos {
			if e.Root == root {
				return nil
			}
		}
		f.Repos = append(f.Repos, Entry{Root: root, TrustedAt: s.now().UTC()})
		return nil
	})
}

// Untrust removes root. It reports whether root was trusted.
func (s *Store) Untrust(root string) (bool, error) {
	var removed bool
	err := s.update(true, func(f *file) error {
		kept := f.Repos[:0]
		for _, e := range f.Repos {
			if e.Root == root {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		f.Repos = kept
		return nil
	})
	return removed, err
}

// List returns trusted repositories sorted by path.
func (s *Store) List() ([]Entry, error) {
	var out []Entry
	err := s.update(false, func(f *file) error {
		out = append(out, f.Repos...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out, err
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
		return fmt.Errorf("read trust store: %w", err)
	default:
		if err := json.Unmarshal(data, f); err != nil {
			return fmt.Errorf("trust store %s is corrupt: %w", s.path, err)
		}
		if f.Version != fileVersion {
			return fmt.Errorf("trust store %s has version %d; this skerry understands %d", s.path, f.Version, fileVersion)
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
