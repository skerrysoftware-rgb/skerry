package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/scope"
	"github.com/skerrysoftware-rgb/skerry/internal/testutil"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

func newManager(t *testing.T) *workspace.Manager {
	t.Helper()
	state := t.TempDir()
	return &workspace.Manager{Registry: registry.Open(state), Scope: scope.Open(state)}
}

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"fix-login", true},
		{"new-session-992dba", true},
		{"a", true},
		{"v1.2_x", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a..b", false},
		{"-flag", false},
		{".hidden", false},
		{"a/b", false},
		{"../escape", false},
		{"with space", false},
		{"semi;colon", false},
		{strings.Repeat("a", 65), false},
	} {
		if err := workspace.ValidateName(tc.name); (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) error = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)

	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "fix-login", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".claude", "worktrees", "fix-login"); ws.Path != want {
		t.Errorf("path = %s, want %s", ws.Path, want)
	}
	if ws.Branch != "worktree-fix-login" || ws.Mode != registry.ModePlain {
		t.Errorf("branch = %s, mode = %s", ws.Branch, ws.Mode)
	}
	if got := testutil.Git(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "worktree-fix-login" {
		t.Errorf("worktree is on %s", got)
	}
	if got := testutil.Git(t, root, "rev-parse", "HEAD"); ws.Base != got {
		t.Errorf("base = %s, want %s", ws.Base, got)
	}
	// The main checkout stays clean: the worktrees directory is excluded.
	if status := testutil.Git(t, root, "status", "--porcelain"); status != "" {
		t.Errorf("main checkout is dirty after create:\n%s", status)
	}
	found, err := m.Find(filepath.Join(ws.Path, "README.md"))
	if err != nil || found.ID != ws.ID {
		t.Errorf("Find inside workspace = %+v, %v", found, err)
	}

	// Asking again for the same name reopens it, as Claude Code does.
	again, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "fix-login"})
	if err != nil || again.ID != ws.ID {
		t.Errorf("second Create = %+v, %v; want the existing workspace", again, err)
	}
	// A directory skerry didn't create is never adopted.
	testutil.WriteFile(t, filepath.Join(root, ".claude", "worktrees", "foreign", "x"), "x")
	if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "foreign"}); err == nil {
		t.Error("Create adopted a directory skerry did not create")
	}
}

func TestCreateFromInsideWorktreeUsesMainRoot(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)
	first, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Create(ctx, workspace.CreateOptions{From: first.Path, Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Repo != root || filepath.Dir(second.Path) != filepath.Dir(first.Path) {
		t.Errorf("nested create landed at %s (repo %s)", second.Path, second.Repo)
	}
}

func TestCreateRefusesSymlinkedWorktreesDir(t *testing.T) {
	root := testutil.NewRepo(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	_, err := newManager(t).Create(context.Background(), workspace.CreateOptions{From: root, Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Create through a symlinked .claude = %v, want symlink refusal", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("Create wrote into the symlink target: %v", entries)
	}
}

func TestModeFollowsScope(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)
	if err := m.Scope.Enable(filepath.Dir(root)); err != nil { // a parent folder
		t.Fatal(err)
	}
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Mode != registry.ModeSkerry {
		t.Errorf("mode under an enabled folder = %s", ws.Mode)
	}
}

func TestGitHooksAreOptIn(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	testutil.WriteFile(t, hook, "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	m := newManager(t)
	if err := m.Scope.Enable(root); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("repository hook ran without opting in")
	}

	m.GitHooks = true
	if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "opted-in"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("repository hook did not run after opting in")
	}
}

func TestFilterDriversDoNotRun(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	testutil.WriteFile(t, filepath.Join(root, ".gitattributes"), "*.txt filter=evil\n")
	testutil.WriteFile(t, filepath.Join(root, "data.txt"), "real content\n")
	testutil.Git(t, root, "add", ".")
	testutil.Git(t, root, "commit", "--quiet", "-m", "data")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	testutil.Git(t, root, "config", "filter.evil.smudge", "touch '"+marker+"'; cat")
	testutil.Git(t, root, "config", "filter.evil.process", "touch '"+marker+"'; false")
	testutil.Git(t, root, "config", "filter.evil.required", "true")

	ws, err := newManager(t).Create(ctx, workspace.CreateOptions{From: root, Name: "filtered"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a filter driver from the repository's config ran")
	}
	if got, _ := os.ReadFile(filepath.Join(ws.Path, "data.txt")); string(got) != "real content\n" {
		t.Errorf("data.txt = %q", got)
	}
}

func TestBaseIsRemoteDefaultBranch(t *testing.T) {
	ctx := context.Background()
	upstream := testutil.NewRepo(t)
	clone := filepath.Join(filepath.Dir(upstream), "clone")
	testutil.Git(t, filepath.Dir(upstream), "clone", "--quiet", upstream, clone)
	remoteHead := testutil.Git(t, clone, "rev-parse", "HEAD")
	testutil.WriteFile(t, filepath.Join(clone, "local.txt"), "unpushed\n")
	testutil.Git(t, clone, "add", "local.txt")
	testutil.Git(t, clone, "commit", "--quiet", "-m", "local only")
	localHead := testutil.Git(t, clone, "rev-parse", "HEAD")

	m := newManager(t)
	fresh, err := m.Create(ctx, workspace.CreateOptions{From: clone, Name: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Base != remoteHead {
		t.Errorf("default base = %s, want origin/HEAD %s", fresh.Base, remoteHead)
	}
	m.BaseFromHead = true
	head, err := m.Create(ctx, workspace.CreateOptions{From: clone, Name: "head"})
	if err != nil {
		t.Fatal(err)
	}
	if head.Base != localHead {
		t.Errorf("head base = %s, want local HEAD %s", head.Base, localHead)
	}
}

func TestCreateFromNamedBase(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	testutil.Git(t, root, "checkout", "--quiet", "-b", "feature")
	testutil.WriteFile(t, filepath.Join(root, "feature.txt"), "feature\n")
	testutil.Git(t, root, "add", "feature.txt")
	testutil.Git(t, root, "commit", "--quiet", "-m", "feature")
	feature := testutil.Git(t, root, "rev-parse", "HEAD")
	testutil.Git(t, root, "checkout", "--quiet", "-")

	m := newManager(t)
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "on-feature", Base: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Base != feature {
		t.Errorf("base = %s, want feature %s", ws.Base, feature)
	}
	for _, bad := range []string{"--output=/tmp/x", "no-such-branch"} {
		if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "bad", Base: bad}); err == nil {
			t.Errorf("base %q accepted", bad)
		}
	}
}

func TestWorktreeIncludeCopiesIgnoredFilesOnly(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	testutil.WriteFile(t, filepath.Join(root, ".gitignore"), ".env\nconfig/secret.json\n")
	testutil.WriteFile(t, filepath.Join(root, ".worktreeinclude"), ".env\nconfig/secret.json\nnotes.txt\n")
	testutil.Git(t, root, "add", ".gitignore", ".worktreeinclude")
	testutil.Git(t, root, "commit", "--quiet", "-m", "include rules")
	testutil.WriteFile(t, filepath.Join(root, ".env"), "TOKEN=abc\n")
	testutil.WriteFile(t, filepath.Join(root, "config", "secret.json"), "{}\n")
	testutil.WriteFile(t, filepath.Join(root, "notes.txt"), "untracked but not ignored\n")

	ws, err := newManager(t).Create(ctx, workspace.CreateOptions{From: root, Name: "inc"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{".env": "TOKEN=abc\n", "config/secret.json": "{}\n"} {
		if got, err := os.ReadFile(filepath.Join(ws.Path, rel)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", rel, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "notes.txt")); err == nil {
		t.Error("copied a file that is not gitignored")
	}
}

func TestRemoveClean(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Remove(ctx, ws, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.BranchKept {
		t.Error("kept a branch with no commits")
	}
	if _, err := os.Stat(ws.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree still exists: %v", err)
	}
	if out := testutil.Git(t, root, "branch", "--list", ws.Branch); out != "" {
		t.Errorf("branch still exists: %s", out)
	}
	if _, err := m.Registry.FindByName(root, "clean"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("registry still has the workspace: %v", err)
	}
}

func TestRemoveRefusesUncommittedChanges(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "dirty"})
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(ws.Path, "new.txt"), "work in progress\n")

	if _, err := m.Remove(ctx, ws, false); !errors.Is(err, workspace.ErrHasWork) {
		t.Fatalf("Remove with uncommitted work = %v, want ErrHasWork", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "new.txt")); err != nil {
		t.Fatal("refused removal still deleted work")
	}
	if _, err := m.Remove(ctx, ws, true); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
}

func TestRemoveKeepsBranchWithCommits(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "committed"})
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(ws.Path, "feature.txt"), "done\n")
	testutil.Git(t, ws.Path, "add", "feature.txt")
	testutil.Git(t, ws.Path, "commit", "--quiet", "-m", "feature")

	if _, err := m.Remove(ctx, ws, false); !errors.Is(err, workspace.ErrHasWork) {
		t.Fatalf("Remove with unpushed commit = %v, want ErrHasWork", err)
	}
	res, err := m.Remove(ctx, ws, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BranchKept {
		t.Error("forced removal deleted a branch with commits")
	}
	if out := testutil.Git(t, root, "branch", "--list", ws.Branch); out == "" {
		t.Error("branch with commits is gone")
	}
}

func TestRemoveAfterManualDelete(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	m := newManager(t)
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ws.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(ctx, ws, false); err != nil {
		t.Fatal(err)
	}
	if out := testutil.Git(t, root, "worktree", "list", "--porcelain"); strings.Contains(out, ws.Path) {
		t.Errorf("git still lists the worktree:\n%s", out)
	}
}

func TestRemoveRefusesPathOutsideWorktreesDir(t *testing.T) {
	root := testutil.NewRepo(t)
	victim := t.TempDir()
	ws := registry.Workspace{ID: "x", Name: "x", Repo: root, Path: victim, Branch: "wip/x"}
	if _, err := newManager(t).Remove(context.Background(), ws, true); err == nil {
		t.Fatal("Remove accepted a path outside the worktrees directory")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("Remove deleted a path outside the worktrees directory")
	}
}

func TestConcurrentCreatesInFreshRepo(t *testing.T) {
	root := testutil.NewRepo(t)
	m := newManager(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Create(context.Background(), workspace.CreateOptions{From: root, Name: fmt.Sprintf("agent-%d", i)})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	exclude, _ := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if n := strings.Count(string(exclude), "/.claude/worktrees/"); n != 1 {
		t.Errorf("exclude has %d worktree entries, want 1:\n%s", n, exclude)
	}
}

func TestAdoptsExistingWorktree(t *testing.T) {
	root := testutil.NewRepo(t)
	path := filepath.Join(root, ".claude", "worktrees", "old")
	testutil.Git(t, root, "worktree", "add", "--quiet", "-b", "worktree-old", path)
	ws, err := newManager(t).Create(context.Background(), workspace.CreateOptions{From: root, Name: "old"})
	if err != nil {
		t.Fatalf("reopening a pre-existing worktree: %v", err)
	}
	if ws.Path != path || ws.Branch != "worktree-old" {
		t.Errorf("adopted %+v", ws)
	}
}

func TestFailedCreateRollsBackBranch(t *testing.T) {
	root := testutil.NewRepo(t)
	m := newManager(t)
	parent := filepath.Join(root, ".claude", "worktrees")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil { // git can't create the worktree
		t.Fatal(err)
	}
	_, err := m.Create(context.Background(), workspace.CreateOptions{From: root, Name: "blocked"})
	os.Chmod(parent, 0o755)
	if err == nil {
		t.Fatal("create into a read-only directory succeeded")
	}
	if out := testutil.Git(t, root, "branch", "--list", "worktree-blocked"); out != "" {
		t.Fatalf("failed create left branch %q behind", out)
	}
	if _, err := m.Create(context.Background(), workspace.CreateOptions{From: root, Name: "blocked"}); err != nil {
		t.Fatalf("retrying the name after a failure: %v", err)
	}
}

type fakeVMs struct{ calls []string }

func (f *fakeVMs) Down(ws registry.Workspace) error {
	f.calls = append(f.calls, "down:"+ws.Name)
	if _, err := os.Stat(ws.Path); err != nil {
		f.calls = append(f.calls, "ERROR worktree already gone at down")
	}
	return nil
}

func (f *fakeVMs) Forget(ws registry.Workspace) error {
	f.calls = append(f.calls, "forget:"+ws.Name)
	return nil
}

func TestRemoveStopsVMFirstAndForgetsIt(t *testing.T) {
	root := testutil.NewRepo(t)
	m := newManager(t)
	vms := &fakeVMs{}
	m.VMs = vms
	if err := m.Scope.Enable(root); err != nil {
		t.Fatal(err)
	}
	ws, err := m.Create(context.Background(), workspace.CreateOptions{From: root, Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(context.Background(), ws, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(vms.calls, ","); got != "down:vm,forget:vm" {
		t.Errorf("VM calls = %s, want down before removal, forget after", got)
	}
}

func TestCanonicalUsesOnDiskCase(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	real := filepath.Join(dir, "CaseRepo")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "caserepo")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	got, err := workspace.Canonical(filepath.Join(dir, "caserepo"))
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Errorf("Canonical = %s, want %s", got, real)
	}
}

// A workspace never gets the main checkout's env files (secrets, often)
// unless the repository's .worktreeinclude lists them: they would be
// readable by code in the VM, and by the agent working in the worktree.
func TestEnvFilesNotCopiedWithoutWorktreeInclude(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	testutil.WriteFile(t, filepath.Join(root, ".gitignore"), ".env\n")
	testutil.Git(t, root, "add", ".gitignore")
	testutil.Git(t, root, "commit", "--quiet", "-m", "ignore env")
	testutil.WriteFile(t, filepath.Join(root, ".env"), "API_KEY=secret\n")
	testutil.WriteFile(t, filepath.Join(root, "packages", "api", ".env"), "PORT=5183\n")

	m := newManager(t)
	if err := m.Scope.Enable(root); err != nil {
		t.Fatal(err)
	}
	ws, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "no-env"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".env", "packages/api/.env"} {
		if _, err := os.Stat(filepath.Join(ws.Path, f)); err == nil {
			t.Errorf("copied %s", f)
		}
	}
}

func TestUnsharedEnvFiles(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	testutil.WriteFile(t, filepath.Join(root, ".gitignore"), ".env\nnode_modules/\n")
	testutil.Git(t, root, "add", ".gitignore")
	testutil.Git(t, root, "commit", "--quiet", "-m", "ignore env")
	testutil.WriteFile(t, filepath.Join(root, "packages", "api", ".env"), "API_KEY=secret\n")
	testutil.WriteFile(t, filepath.Join(root, "node_modules", "x", ".env"), "NOPE=1\n")
	m := newManager(t)

	got, err := m.UnsharedEnvFiles(ctx, root)
	if err != nil || len(got) != 1 || got[0] != "packages/api/.env" {
		t.Errorf("unshared = %v, %v", got, err)
	}
	// Listed in .worktreeinclude: shared, so nothing to say.
	testutil.WriteFile(t, filepath.Join(root, ".worktreeinclude"), "**/.env\n")
	if got, err := m.UnsharedEnvFiles(ctx, root); err != nil || len(got) != 0 {
		t.Errorf("unshared with .worktreeinclude = %v, %v", got, err)
	}
}
