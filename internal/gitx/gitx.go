// Package gitx runs the git operations skerry needs. Every call goes through
// one runner: arguments are passed directly (never through a shell) and
// git's own environment overrides are cleared so a caller's GIT_DIR cannot
// redirect an operation.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Git runs git commands.
type Git struct {
	// Bin is the git executable; empty means "git" from PATH.
	Bin string
}

// Error is a git command that ran and exited non-zero.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	msg := e.Stderr
	if msg == "" {
		msg = "exit status " + strconv.Itoa(e.ExitCode)
	}
	return "git " + strings.Join(e.Args, " ") + ": " + msg
}

// exitedWith reports whether err is a git command exiting with code.
func exitedWith(err error, code int) bool {
	var gerr *Error
	return errors.As(err, &gerr) && gerr.ExitCode == code
}

// run executes git in dir and returns trimmed stdout.
func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", &Error{Args: args, ExitCode: exitErr.ExitCode(), Stderr: strings.TrimSpace(stderr.String())}
		}
		return "", fmt.Errorf("run git: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// cleanEnv drops variables that would point git at a different repository,
// and stops git from prompting for credentials.
func cleanEnv(env []string) []string {
	drop := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_CEILING_DIRECTORIES": true,
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

// MainRoot returns the root of the main checkout for dir, even when dir is
// inside a linked worktree.
func (g Git) MainRoot(ctx context.Context, dir string) (string, error) {
	common, err := g.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	if filepath.Base(common) != ".git" {
		return "", fmt.Errorf("unsupported repository layout (git dir %s); skerry needs a normal checkout with a .git directory", common)
	}
	return filepath.Dir(common), nil
}

// Head returns the full commit hash HEAD points at in dir.
func (g Git) Head(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
}

// BranchExists reports whether a local branch exists.
func (g Git) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	_, err := g.run(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	switch {
	case err == nil:
		return true, nil
	case exitedWith(err, 1):
		return false, nil
	default:
		return false, err
	}
}

// ValidBranchName reports whether git accepts name as a branch name.
func (g Git) ValidBranchName(ctx context.Context, repo, name string) bool {
	_, err := g.run(ctx, repo, "check-ref-format", "--branch", name)
	return err == nil
}

// AddWorktreeOptions describes a new worktree.
type AddWorktreeOptions struct {
	// From is a directory in the repository; the new branch starts at its HEAD.
	From string
	// Path is the new worktree's directory. It must not exist.
	Path string
	// Branch is created for the worktree.
	Branch string
	// Base is the commit the branch starts at.
	Base string
	// RunHooks allows the repository's git hooks (such as post-checkout) to
	// run. skerry only allows it for trusted repositories.
	RunHooks bool
}

// AddWorktree creates a worktree on a new branch.
func (g Git) AddWorktree(ctx context.Context, o AddWorktreeOptions) error {
	args := []string{}
	if !o.RunHooks {
		args = append(args, "-c", "core.hooksPath="+os.DevNull)
	}
	args = append(args, "worktree", "add", "--quiet", "-b", o.Branch, "--", o.Path, o.Base)
	_, err := g.run(ctx, o.From, args...)
	return err
}

// Dirty reports whether a worktree has uncommitted changes or untracked,
// non-ignored files.
func (g Git) Dirty(ctx context.Context, path string) (bool, error) {
	out, err := g.run(ctx, path, "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Unpushed counts commits made since base that no remote has.
func (g Git) Unpushed(ctx context.Context, path, base string) (int, error) {
	out, err := g.run(ctx, path, "rev-list", "--count", base+"..HEAD", "--not", "--remotes")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// RemoveWorktree removes a worktree. force discards uncommitted changes.
func (g Git) RemoveWorktree(ctx context.Context, repo, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force", "--force")
	}
	_, err := g.run(ctx, repo, append(args, "--", path)...)
	return err
}

// PruneWorktrees drops git's records of worktrees whose directories are gone.
func (g Git) PruneWorktrees(ctx context.Context, repo string) error {
	_, err := g.run(ctx, repo, "worktree", "prune")
	return err
}

// DeleteBranch deletes a local branch regardless of merge state. Callers
// must first check that it holds nothing worth keeping.
func (g Git) DeleteBranch(ctx context.Context, repo, branch string) error {
	_, err := g.run(ctx, repo, "branch", "-D", "--", branch)
	return err
}

// BranchHead returns the commit a local branch points at.
func (g Git) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	return g.run(ctx, repo, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
}

// ExcludeLocally makes git ignore pattern in this clone only, by appending it
// to .git/info/exclude unless the path is already ignored.
func (g Git) ExcludeLocally(ctx context.Context, repo, probePath, pattern string) error {
	switch _, err := g.run(ctx, repo, "check-ignore", "--quiet", "--no-index", "--", probePath); {
	case err == nil:
		return nil // already ignored
	case !exitedWith(err, 1):
		return err
	}
	gitDir, err := g.run(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	exclude := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	existing, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		existing = append(existing, '\n')
	}
	existing = append(existing, []byte("# added by skerry: worktrees live here\n"+pattern+"\n")...)
	return os.WriteFile(exclude, existing, 0o644)
}
