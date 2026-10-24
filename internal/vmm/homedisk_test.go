package vmm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

// The home disk (caches) is the workspace's own: a reset replaces the
// service data and keeps it, and a snapshot carries the data disk only.
func TestHomeDiskStaysOutOfResetAndSnapshots(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	m := &Manager{Dirs: paths.Dirs{State: filepath.Join(base, "state"), Cache: filepath.Join(base, "cache")}}
	ws := registry.Workspace{ID: "abc123", Name: "w", Repo: filepath.Join(base, "repo"), Path: filepath.Join(base, "repo", "w")}
	p, err := m.paths(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.state, filepath.Join(ws.Repo, ".git")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p.dataDisk, []byte("service data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.homeDisk, []byte("written by the guest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveSnapshot(ws, "base", nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(m.snapshotDir(ws.Repo), "base.img")); string(data) != "service data" {
		t.Errorf("snapshot holds %q, not the data disk", data)
	}
	entries, _ := os.ReadDir(m.snapshotDir(ws.Repo))
	for _, e := range entries {
		if e.Name() != "base.img" && e.Name() != "base.json" && e.Name() != ".lock" {
			t.Errorf("snapshot has %s", e.Name())
		}
	}
	if err := m.ResetData(ws, "", false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p.homeDisk); string(data) != "written by the guest" {
		t.Errorf("reset touched the home disk: %q", data)
	}
	if info, err := os.Stat(p.dataDisk); err != nil || info.Size() != dataDiskSize {
		t.Errorf("reset didn't replace the data disk with an empty one: %v", err)
	}
}
