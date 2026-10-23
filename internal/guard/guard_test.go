package guard_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/guard"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/scope"
	"github.com/skerrysoftware-rgb/skerry/internal/testutil"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

func skerryWorkspace(t *testing.T) registry.Workspace {
	t.Helper()
	root := testutil.NewRepo(t)
	testutil.WriteFile(t, filepath.Join(root, ".claude", "settings.json"), "{}\n")
	testutil.Git(t, root, "add", ".claude/settings.json")
	testutil.Git(t, root, "commit", "--quiet", "-m", "claude settings")
	state := t.TempDir()
	m := &workspace.Manager{Registry: registry.Open(state), Scope: scope.Open(state)}
	if err := m.Scope.Enable(root); err != nil {
		t.Fatal(err)
	}
	ws, err := m.Create(context.Background(), workspace.CreateOptions{From: root, Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestPointerIsProtectedAndChecked(t *testing.T) {
	ws := skerryWorkspace(t)
	if ws.GitDir == "" || !strings.HasPrefix(ws.GitDir, filepath.Join(ws.Repo, ".git", "worktrees")) {
		t.Fatalf("GitDir = %q", ws.GitDir)
	}
	if err := guard.CheckPointer(ws); err != nil {
		t.Fatalf("fresh workspace: %v", err)
	}
	pointer := filepath.Join(ws.Path, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: /tmp/evil\n"), 0o644); err == nil {
		t.Fatal("overwrote the protected .git pointer")
	}
	if err := os.Rename(pointer, pointer+".moved"); err == nil {
		t.Fatal("renamed the protected .git pointer")
	}

	// With protection off, a changed pointer is detected.
	if err := guard.Unprotect(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, []byte("gitdir: /tmp/evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := guard.CheckPointer(ws); err == nil {
		t.Fatal("a redirected pointer passed the check")
	}
}

func TestScanQuarantinesPlantedFiles(t *testing.T) {
	ws := skerryWorkspace(t)
	plant := func(rel, content string) string {
		p := filepath.Join(ws.Path, rel)
		testutil.WriteFile(t, p, content)
		return p
	}
	nested := plant("pkg/a/.git/config", "[core]\n\tfsmonitor = touch /tmp/pwned\n")
	foreign := plant("pkg/b/.git", "gitdir: /tmp/attacker-repo\n")
	submodule := plant("pkg/c/.git", "gitdir: "+filepath.Join(ws.Repo, ".git", "modules", "c")+"\n")
	bare := filepath.Join(ws.Path, "pkg", "bare")
	plant("pkg/bare/HEAD", "ref: refs/heads/main\n")
	plant("pkg/bare/objects/.keep", "")
	plant("pkg/bare/refs/.keep", "")
	local := plant(".claude/settings.local.json", `{"hooks":{}}`)
	mcp := plant(".mcp.json", `{"mcpServers":{}}`)
	inNodeModules := plant("node_modules/x/.git/config", "")

	found, err := guard.Scan(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	gone := []string{filepath.Dir(nested), foreign, bare, local, mcp}
	for _, p := range gone {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was not quarantined", p)
		}
	}
	for _, p := range []string{submodule, inNodeModules, filepath.Join(ws.Path, ".claude", "settings.json")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was moved but should be left alone", p)
		}
	}
	if len(found) != len(gone) {
		t.Errorf("found %d items, want %d: %v", len(found), len(gone), found)
	}
	for _, f := range found {
		if _, err := os.Lstat(f.Quarantined); err != nil {
			t.Errorf("quarantined copy of %s is missing", f.Path)
		}
	}
}

func TestRemoveRefusesTamperedPointer(t *testing.T) {
	ws := skerryWorkspace(t)
	state := t.TempDir()
	m := &workspace.Manager{Registry: registry.Open(state), Scope: scope.Open(state)}
	if err := m.Registry.Add(ws); err != nil {
		t.Fatal(err)
	}
	if err := guard.Unprotect(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, ".git"), []byte("gitdir: /tmp/evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(context.Background(), ws, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Remove with a tampered pointer = %v, want a refusal", err)
	}
}
