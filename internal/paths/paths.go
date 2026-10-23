// Package paths locates skerry's state and cache directories.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Dirs holds skerry's per-user directories. Both are created with mode 0700.
type Dirs struct {
	// State holds the registry, trust store and per-workspace data.
	State string
	// Cache holds downloads and built images that can be recreated.
	Cache string
}

// Default returns the directories for the current user. SKERRY_HOME, when
// set, puts both under one root, which tests and portable setups use.
func Default() (Dirs, error) {
	if home := os.Getenv("SKERRY_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			return Dirs{}, fmt.Errorf("SKERRY_HOME must be an absolute path, got %q", home)
		}
		return Dirs{State: filepath.Join(home, "state"), Cache: filepath.Join(home, "cache")}, nil
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("locate cache directory: %w", err)
	}
	state, err := stateBase()
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{State: filepath.Join(state, "skerry"), Cache: filepath.Join(cache, "skerry")}, nil
}

func stateBase() (string, error) {
	if runtime.GOOS == "darwin" {
		// ~/Library/Application Support
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate application support directory: %w", err)
		}
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

// Ensure creates both directories with mode 0700 and refuses to use existing
// ones that other users can read or write.
func (d Dirs) Ensure() error {
	for _, dir := range []string{d.State, d.Cache} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New(dir + " is accessible by other users; run: chmod 700 " + dir)
	}
	return nil
}
