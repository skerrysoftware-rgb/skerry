// Package guard protects the host from files a workspace VM writes into the
// shared checkout.
//
// The checkout is shared read-write, so code in the VM can write files that
// tools on the host later trust. The most dangerous are git's own: a
// redirected .git pointer, or a planted nested repository, has git on the
// host read a config the VM controls, and git config can run programs. Next
// are files Claude Code loads as configuration. guard keeps the worktree's
// .git pointer immutable, lets skerry check it, and finds and quarantines
// planted repositories and Claude settings.
package guard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/gitx"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

// Protect makes the worktree's .git pointer immutable. Git never rewrites it,
// so this costs nothing in normal use.
func Protect(ws registry.Workspace) error {
	return fsutil.SetImmutable(filepath.Join(ws.Path, ".git"), true)
}

// Unprotect clears the flag so the worktree can be removed.
func Unprotect(ws registry.Workspace) error {
	err := fsutil.SetImmutable(filepath.Join(ws.Path, ".git"), false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ErrTampered means the worktree's .git pointer no longer points where it
// did when skerry created the worktree.
var ErrTampered = errors.New("the worktree's .git pointer has been changed")

// CheckPointer verifies the worktree's .git is still a plain file pointing
// at the git directory recorded when the worktree was created.
func CheckPointer(ws registry.Workspace) error {
	if ws.GitDir == "" {
		return nil // recorded before skerry tracked it
	}
	p := filepath.Join(ws.Path, ".git")
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTampered, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a plain file", ErrTampered, p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "gitdir: "+ws.GitDir {
		return fmt.Errorf("%w: %s now reads %q", ErrTampered, p, strings.TrimSpace(string(data)))
	}
	return nil
}

// Git returns a git runner that uses the workspace's recorded git directory
// explicitly, so nothing planted in the checkout can redirect it.
func Git(ws registry.Workspace) gitx.Git {
	if ws.GitDir == "" {
		return gitx.Git{}
	}
	return gitx.Git{GitDir: ws.GitDir, WorkTree: ws.Path}
}

// Finding is something the scan found and dealt with.
type Finding struct {
	Path string
	What string
	// Quarantined is where it was moved; empty if only reported.
	Quarantined string
}

func (f Finding) String() string {
	if f.Quarantined != "" {
		return fmt.Sprintf("%s: %s; moved to %s", f.Path, f.What, filepath.Base(f.Quarantined))
	}
	return fmt.Sprintf("%s: %s", f.Path, f.What)
}

// Scan limits.
const (
	scanDepth  = 12
	scanBudget = 2 * time.Second
)

// Scan looks for files planted in the checkout that host tools would trust,
// quarantines them by renaming, and returns what it found:
//
//   - nested repositories: a .git directory, a .git file pointing outside the
//     main repository (submodules point inside it and are left alone), or a
//     bare repository, any of which git on the host would use when run from
//     that folder
//   - Claude Code settings or MCP config that git doesn't track: Claude would
//     load them, and they can name commands to run
//
// node_modules is skipped for speed; host tools rarely run git inside it.
func Scan(ctx context.Context, ws registry.Workspace) ([]Finding, error) {
	var found []Finding
	deadline := time.Now().Add(scanBudget)
	trustedGitDirs := filepath.Join(ws.Repo, ".git") + string(filepath.Separator)

	err := filepath.WalkDir(ws.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries can't be used by host git either
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(ws.Path, path)
		if rel == "." {
			return nil
		}
		if strings.Count(rel, string(filepath.Separator)) >= scanDepth {
			return fs.SkipDir
		}
		name := d.Name()
		if d.IsDir() && name == "node_modules" {
			return fs.SkipDir
		}
		if rel == ".git" {
			return nil // the worktree's own pointer; CheckPointer covers it
		}
		switch {
		case name == ".git" && d.IsDir():
			found = append(found, quarantine(path, "a nested git repository"))
			return fs.SkipDir
		case name == ".git" && d.Type().IsRegular():
			data, err := os.ReadFile(path)
			target := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
			if err != nil || !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			if !strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator), trustedGitDirs) {
				found = append(found, quarantine(path, "a .git file pointing outside the repository"))
			}
		case d.IsDir() && isBareRepo(path):
			found = append(found, quarantine(path, "a bare git repository"))
			return fs.SkipDir
		case d.Type()&fs.ModeSymlink != 0 && name == ".git":
			found = append(found, quarantine(path, "a .git symlink"))
		}
		return nil
	})
	if err != nil {
		return found, err
	}

	untracked, err := Git(ws).Untracked(ctx, ws.Path, ".claude/settings.json", ".claude/settings.local.json", ".mcp.json")
	if err != nil {
		return found, err
	}
	for _, rel := range untracked {
		found = append(found, quarantine(filepath.Join(ws.Path, rel), "Claude Code configuration that git doesn't track"))
	}
	return found, nil
}

func isBareRepo(dir string) bool {
	for _, p := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err != nil {
			return false
		}
	}
	return true
}

// quarantine renames path out of the way, so nothing loads it, and keeps it
// for the user to inspect.
func quarantine(path, what string) Finding {
	dest := path + ".skerry-quarantine-" + strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.Rename(path, dest); err != nil {
		return Finding{Path: path, What: what + " (could not move it: " + err.Error() + ")"}
	}
	return Finding{Path: path, What: what, Quarantined: dest}
}
