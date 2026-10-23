package registry_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

func TestFindByPathPicksInnermost(t *testing.T) {
	r := registry.Open(t.TempDir())
	outer := registry.Workspace{ID: "1", Name: "outer", Repo: "/r", Path: "/r/.claude/worktrees/outer"}
	inner := registry.Workspace{ID: "2", Name: "inner", Repo: "/r", Path: "/r/.claude/worktrees/outer/nested"}
	sibling := registry.Workspace{ID: "3", Name: "outer2", Repo: "/r", Path: "/r/.claude/worktrees/outer2"}
	for _, ws := range []registry.Workspace{outer, inner, sibling} {
		if err := r.Add(ws); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		"/r/.claude/worktrees/outer":             "1",
		"/r/.claude/worktrees/outer/src/a.go":    "1",
		"/r/.claude/worktrees/outer/nested/x":    "2",
		"/r/.claude/worktrees/outer2/README.md":  "3",
		"/r/.claude/worktrees/outer2-not-ours/x": "",
		"/r/.claude/worktrees":                   "",
	} {
		got, err := r.FindByPath(path)
		switch {
		case want == "" && !errors.Is(err, registry.ErrNotFound):
			t.Errorf("FindByPath(%s) = %v, %v; want not found", path, got.ID, err)
		case want != "" && (err != nil || got.ID != want):
			t.Errorf("FindByPath(%s) = %v, %v; want %s", path, got.ID, err, want)
		}
	}
}

func TestAddRejectsDuplicates(t *testing.T) {
	r := registry.Open(t.TempDir())
	ws := registry.Workspace{ID: "1", Name: "a", Repo: "/r", Path: "/r/.claude/worktrees/a"}
	if err := r.Add(ws); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(registry.Workspace{ID: "2", Name: "a", Repo: "/r", Path: "/elsewhere"}); err == nil {
		t.Error("duplicate name in one repository accepted")
	}
	if err := r.Add(registry.Workspace{ID: "3", Name: "b", Repo: "/r2", Path: ws.Path}); err == nil {
		t.Error("duplicate path accepted")
	}
	if err := r.Add(registry.Workspace{ID: "4", Name: "a", Repo: "/r2", Path: "/r2/.claude/worktrees/a"}); err != nil {
		t.Errorf("same name in another repository rejected: %v", err)
	}
	if _, err := r.FindByName("", "a"); err == nil {
		t.Error("ambiguous name lookup succeeded")
	}
}

func TestConcurrentAddsAreNotLost(t *testing.T) {
	dir := t.TempDir()
	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A separate handle per goroutine, like separate processes.
			r := registry.Open(dir)
			name := fmt.Sprintf("ws%d", i)
			errs <- r.Add(registry.Workspace{ID: name, Name: name, Repo: "/r", Path: "/r/" + name})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := registry.Open(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("registry has %d workspaces, want %d", len(all), n)
	}
}

func TestFilePermissionsAndCorruption(t *testing.T) {
	dir := t.TempDir()
	r := registry.Open(dir)
	if err := r.Add(registry.Workspace{ID: "1", Name: "a", Repo: "/r", Path: "/r/a"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry mode = %o, want 600", perm)
	}

	if err := os.WriteFile(filepath.Join(dir, "workspaces.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(); err == nil {
		t.Error("corrupt registry read without error")
	}
}
