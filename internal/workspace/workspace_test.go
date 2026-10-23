package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/testutil"
	"github.com/skerrysoftware-rgb/skerry/internal/trust"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

func newManager(t *testing.T) *workspace.Manager {
	t.Helper()
	state := t.TempDir()
	return &workspace.Manager{Registry: registry.Open(state), Trust: trust.Open(state)}
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
	if ws.Branch != "wip/fix-login" {
		t.Errorf("branch = %s", ws.Branch)
	}
	if got := testutil.Git(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "wip/fix-login" {
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

	if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "fix-login"}); err == nil {
		t.Error("creating the same name twice succeeded")
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

func TestCreateRunsRepoHooksOnlyWhenTrusted(t *testing.T) {
	ctx := context.Background()
	root := testutil.NewRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	testutil.WriteFile(t, hook, "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	m := newManager(t)

	if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "untrusted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("repository hook ran for an untrusted repository")
	}

	if err := m.Trust.Trust(root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, workspace.CreateOptions{From: root, Name: "trusted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("repository hook did not run for a trusted repository")
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
