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
	// GitDir and WorkTree, when set, are passed explicitly, so git never
	// discovers a repository from the working directory. Workspace commands
	// use them: a VM can write into the checkout, including its .git entry.
	GitDir, WorkTree string
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
	out, err := g.runInput(ctx, dir, nil, args...)
	return strings.TrimSpace(out), err
}

// runInput executes git in dir with stdin and returns raw stdout.
func (g Git) runInput(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	pre := []string{"-C", dir}
	if g.GitDir != "" {
		pre = append(pre, "--git-dir="+g.GitDir, "--work-tree="+g.WorkTree)
	}
	cmd := exec.CommandContext(ctx, bin, append(pre, args...)...)
	cmd.Env = cleanEnv(os.Environ())
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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
	return stdout.String(), nil
}

// guard returns -c overrides that stop git from running programs named in the
// repository's own config while it checks out or inspects files: filter
// drivers (such as a local Git LFS install), the file-system monitor, and,
// unless allowHooks, the repository's hooks. Anything that can write to the
// repository, an agent included, could have planted those.
func (g Git) guard(ctx context.Context, repo string, allowHooks bool) ([]string, error) {
	args := []string{"-c", "core.fsmonitor=false"}
	if !allowHooks {
		args = append(args, "-c", "core.hooksPath="+os.DevNull)
	}
	out, err := g.run(ctx, repo, "config", "--local", "--includes", "--name-only", "--get-regexp", `^filter\.`)
	if err != nil && !exitedWith(err, 1) { // 1: no filters configured
		return nil, fmt.Errorf("read repository git config: %w", err)
	}
	seen := map[string]bool{}
	for _, key := range strings.Fields(out) {
		// filter.<name>.<variable>; the name itself may contain dots.
		dot := strings.LastIndex(key, ".")
		name := key[len("filter."):max(dot, len("filter."))]
		if name == "" || seen[name] {
			continue
		}
		if strings.ContainsAny(name, "=\n") {
			return nil, fmt.Errorf("the repository's git config defines filter %q, which skerry cannot switch off", name)
		}
		seen[name] = true
		args = append(args,
			"-c", "filter."+name+".process=",
			"-c", "filter."+name+".smudge=",
			"-c", "filter."+name+".clean=",
			"-c", "filter."+name+".required=false",
		)
	}
	return args, nil
}

// DefaultBase returns the commit a new worktree starts from, as Claude Code
// does by default: the remote's default branch, or HEAD of from when there
// is no cached remote default.
func (g Git) DefaultBase(ctx context.Context, from string) (string, error) {
	if base, err := g.run(ctx, from, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD^{commit}"); err == nil && base != "" {
		return base, nil
	}
	return g.Head(ctx, from)
}

// WorktreeIncludes lists untracked, gitignored files in the main checkout
// that match the repository's .worktreeinclude patterns, relative to root.
// Claude Code copies these (such as .env) into each new worktree.
func (g Git) WorktreeIncludes(ctx context.Context, root string) ([]string, error) {
	patterns := filepath.Join(root, ".worktreeinclude")
	if info, err := os.Lstat(patterns); err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	guard, err := g.guard(ctx, root, false)
	if err != nil {
		return nil, err
	}
	candidates, err := g.runInput(ctx, root, nil, append(guard, "ls-files", "-z", "--others", "--ignored", "--exclude-from="+patterns)...)
	if err != nil || candidates == "" {
		return nil, err
	}
	ignored, err := g.runInput(ctx, root, []byte(candidates), append(guard, "check-ignore", "-z", "--stdin")...)
	if err != nil && !exitedWith(err, 1) { // 1: none of them are ignored
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(ignored, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
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

// Commit resolves ref (a branch, tag or commit) to a commit id.
func (g Git) Commit(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	return g.run(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

// AbsoluteGitDir returns the git directory for the checkout at dir.
func (g Git) AbsoluteGitDir(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--absolute-git-dir")
}

// Untracked lists files under the given paths that git doesn't track,
// including ignored ones, relative to the work tree.
func (g Git) Untracked(ctx context.Context, dir string, paths ...string) ([]string, error) {
	guard, err := g.guard(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	out, err := g.runInput(ctx, dir, nil, append(append(guard, "ls-files", "-z", "--others", "--"), paths...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// TopLevel returns the top directory of the working tree containing dir.
func (g Git) TopLevel(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--show-toplevel")
}

// CurrentBranch returns the branch checked out in dir, or "HEAD" when
// detached.
func (g Git) CurrentBranch(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
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
	// RunHooks lets the repository's git hooks (such as post-checkout) run.
	// Off unless the user opts in.
	RunHooks bool
}

// AddWorktree creates a worktree on a new branch, with the repository's
// filter drivers switched off (see guard).
func (g Git) AddWorktree(ctx context.Context, o AddWorktreeOptions) error {
	args, err := g.guard(ctx, o.From, o.RunHooks)
	if err != nil {
		return err
	}
	args = append(args, "worktree", "add", "--quiet", "-b", o.Branch, "--", o.Path, o.Base)
	_, err = g.run(ctx, o.From, args...)
	return err
}

// Dirty reports whether a worktree has uncommitted changes or untracked,
// non-ignored files.
func (g Git) Dirty(ctx context.Context, path string) (bool, error) {
	guard, err := g.guard(ctx, path, false)
	if err != nil {
		return false, err
	}
	out, err := g.run(ctx, path, append(guard, "status", "--porcelain=v1", "--untracked-files=normal")...)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Unpushed counts commits made since base that no remote has.
func (g Git) Unpushed(ctx context.Context, path, base string) (int, error) {
	guard, err := g.guard(ctx, path, false)
	if err != nil {
		return 0, err
	}
	out, err := g.run(ctx, path, append(guard, "rev-list", "--count", base+"..HEAD", "--not", "--remotes")...)
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
