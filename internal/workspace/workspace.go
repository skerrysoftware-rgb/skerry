// Package workspace creates and removes skerry workspaces.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/gitx"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/trust"
)

// WorktreesDir is where workspaces live inside a repository. It matches
// Claude Code's default, so its tools find them where they expect.
const WorktreesDir = ".claude/worktrees"

// DefaultBranchPrefix starts the branch for each new workspace. Branches are
// meant to be renamed before they are pushed.
const DefaultBranchPrefix = "wip/"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateName checks a workspace name. Names become directory and branch
// names, so anything that could escape a directory is refused.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid workspace name %q: use 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

// Manager creates and removes workspaces.
type Manager struct {
	Registry     *registry.Registry
	Trust        *trust.Store
	Git          gitx.Git
	BranchPrefix string
	Now          func() time.Time
}

// CreateOptions describes a new workspace.
type CreateOptions struct {
	// From is a directory in the repository; the workspace starts at its HEAD.
	From string
	// Name is the workspace name.
	Name string
	// CreatedBy records what asked for it, e.g. "cli" or "claude".
	CreatedBy string
}

// Create makes a worktree on a new branch and registers it.
func (m *Manager) Create(ctx context.Context, o CreateOptions) (registry.Workspace, error) {
	if err := ValidateName(o.Name); err != nil {
		return registry.Workspace{}, err
	}
	from, err := Canonical(o.From)
	if err != nil {
		return registry.Workspace{}, err
	}
	root, err := m.Git.MainRoot(ctx, from)
	if err != nil {
		return registry.Workspace{}, err
	}
	if root, err = Canonical(root); err != nil {
		return registry.Workspace{}, err
	}

	parent, err := ensureRealDir(root, WorktreesDir)
	if err != nil {
		return registry.Workspace{}, err
	}
	path := filepath.Join(parent, o.Name)
	if _, err := os.Lstat(path); err == nil {
		return registry.Workspace{}, fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return registry.Workspace{}, err
	}

	prefix := m.BranchPrefix
	if prefix == "" {
		prefix = DefaultBranchPrefix
	}
	branch := prefix + o.Name
	if !m.Git.ValidBranchName(ctx, root, branch) {
		return registry.Workspace{}, fmt.Errorf("%q is not a valid branch name", branch)
	}
	if exists, err := m.Git.BranchExists(ctx, root, branch); err != nil {
		return registry.Workspace{}, err
	} else if exists {
		return registry.Workspace{}, fmt.Errorf("branch %s already exists; pick another workspace name", branch)
	}

	base, err := m.Git.Head(ctx, from)
	if err != nil {
		return registry.Workspace{}, fmt.Errorf("find starting commit: %w", err)
	}
	trusted, err := m.Trust.IsTrusted(root)
	if err != nil {
		return registry.Workspace{}, err
	}
	if err := m.Git.AddWorktree(ctx, gitx.AddWorktreeOptions{
		From: from, Path: path, Branch: branch, Base: base, RunHooks: trusted,
	}); err != nil {
		return registry.Workspace{}, err
	}
	// Keep the main checkout's `git status` clean of the worktrees directory.
	if err := m.Git.ExcludeLocally(ctx, root, WorktreesDir+"/"+o.Name, "/"+WorktreesDir+"/"); err != nil {
		return registry.Workspace{}, m.rollback(ctx, root, path, branch, err)
	}

	id, err := newID()
	if err != nil {
		return registry.Workspace{}, m.rollback(ctx, root, path, branch, err)
	}
	ws := registry.Workspace{
		ID: id, Name: o.Name, Repo: root, Path: path, Branch: branch, Base: base,
		CreatedBy: o.CreatedBy, CreatedAt: m.now().UTC(),
	}
	if err := m.Registry.Add(ws); err != nil {
		return registry.Workspace{}, m.rollback(ctx, root, path, branch, err)
	}
	return ws, nil
}

// rollback undoes a half-created worktree and returns the original error.
func (m *Manager) rollback(ctx context.Context, root, path, branch string, cause error) error {
	errs := []error{cause}
	if err := m.Git.RemoveWorktree(ctx, root, path, true); err != nil {
		errs = append(errs, fmt.Errorf("rollback: %w", err))
	}
	if err := m.Git.DeleteBranch(ctx, root, branch); err != nil {
		errs = append(errs, fmt.Errorf("rollback: %w", err))
	}
	return errors.Join(errs...)
}

// ErrHasWork means removing a workspace would lose uncommitted changes or
// commits that exist nowhere else.
var ErrHasWork = errors.New("workspace has work that would be lost")

// RemoveResult says what Remove did with the workspace's branch.
type RemoveResult struct {
	Branch     string
	BranchKept bool
}

// Remove deletes a workspace's worktree and registry entry. Without force it
// refuses when there are uncommitted changes or unpushed commits. It never
// deletes a branch that has commits of its own, even with force.
func (m *Manager) Remove(ctx context.Context, ws registry.Workspace, force bool) (RemoveResult, error) {
	// The registry is the source of truth, but check the invariant anyway:
	// only ever delete inside the repository's worktrees directory.
	if filepath.Dir(ws.Path) != filepath.Join(ws.Repo, WorktreesDir) || ValidateName(filepath.Base(ws.Path)) != nil {
		return RemoveResult{}, fmt.Errorf("refusing to remove %s: not a skerry worktree path", ws.Path)
	}

	_, statErr := os.Lstat(ws.Path)
	switch {
	case statErr == nil:
		if !force {
			if err := m.checkNoWork(ctx, ws); err != nil {
				return RemoveResult{}, err
			}
		}
		if err := m.Git.RemoveWorktree(ctx, ws.Repo, ws.Path, force); err != nil {
			return RemoveResult{}, err
		}
	case errors.Is(statErr, os.ErrNotExist):
		// Already deleted by hand; drop git's record of it.
		if err := m.Git.PruneWorktrees(ctx, ws.Repo); err != nil {
			return RemoveResult{}, err
		}
	default:
		return RemoveResult{}, statErr
	}

	res := RemoveResult{Branch: ws.Branch}
	head, err := m.Git.BranchHead(ctx, ws.Repo, ws.Branch)
	switch {
	case err != nil:
		// The branch is already gone.
	case head == ws.Base:
		if err := m.Git.DeleteBranch(ctx, ws.Repo, ws.Branch); err != nil {
			return res, err
		}
	default:
		res.BranchKept = true
	}
	if err := m.Registry.Remove(ws.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return res, err
	}
	return res, nil
}

func (m *Manager) checkNoWork(ctx context.Context, ws registry.Workspace) error {
	dirty, err := m.Git.Dirty(ctx, ws.Path)
	if err != nil {
		return err
	}
	unpushed, err := m.Git.Unpushed(ctx, ws.Path, ws.Base)
	if err != nil {
		return err
	}
	var problems []string
	if dirty {
		problems = append(problems, "uncommitted changes")
	}
	if unpushed > 0 {
		problems = append(problems, fmt.Sprintf("%d unpushed commit(s)", unpushed))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s has %s; commit and push, or run: skerry rm --force %s",
			ErrHasWork, ws.Path, strings.Join(problems, " and "), ws.Name)
	}
	return nil
}

// Find returns the workspace containing path.
func (m *Manager) Find(path string) (registry.Workspace, error) {
	p, err := Canonical(path)
	if err != nil {
		return registry.Workspace{}, err
	}
	return m.Registry.FindByPath(p)
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Canonical returns an absolute path with symlinks resolved.
func Canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return real, nil
}

// ensureRealDir creates root/rel and makes sure no part of rel is a symlink:
// a repository could commit .claude as a symlink to redirect worktrees, and
// later removals, somewhere else.
func ensureRealDir(root, rel string) (string, error) {
	dir := root
	for _, part := range strings.Split(rel, "/") {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("refusing to use %s: it is a symlink", dir)
		case !info.IsDir():
			return "", fmt.Errorf("refusing to use %s: not a directory", dir)
		}
	}
	return dir, nil
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
