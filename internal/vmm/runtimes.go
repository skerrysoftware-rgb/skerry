package vmm

import (
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

// Which runtimes' commands run in the VM is decided from skerry.yaml,
// which code in the VM can edit. Removing a runtime from it must not move
// that runtime's commands back onto the host (where a planted conftest.py
// or setup.py would then run), so skerry keeps its own record of every
// runtime a workspace has had. The record only grows; it goes with the
// workspace.

// noteRuntimes adds the project's runtimes to the workspace's record.
func (m *Manager) noteRuntimes(ws registry.Workspace, proj project.Project) {
	p, err := m.paths(ws)
	if err != nil {
		return
	}
	have := m.RecordedRuntimes(ws)
	changed := false
	for _, r := range proj.Runtimes {
		if !slices.Contains(have, r.Name) {
			have = append(have, r.Name)
			changed = true
		}
	}
	if changed {
		sort.Strings(have)
		_ = fsutil.WriteFileAtomic(p.runtimes, []byte(strings.Join(have, "\n")+"\n"), 0o600)
	}
}

// RecordedRuntimes lists every runtime the workspace has had.
func (m *Manager) RecordedRuntimes(ws registry.Workspace) []string {
	p, err := m.paths(ws)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p.runtimes)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

// imageSpec is a project's image contents as one comparable string.
func imageSpec(proj project.Project) string {
	pkgs := slices.Clone(ImagePackages(proj))
	sort.Strings(pkgs)
	return strings.Join(slices.Compact(pkgs), " ")
}
