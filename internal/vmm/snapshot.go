package vmm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

// A snapshot is a saved copy of a workspace's data disk: databases already
// seeded (from a production dump, say) and migrated. Snapshots belong to a
// repository. New workspaces of the repository start from its default
// snapshot, as an APFS clone that takes no space until it diverges, and
// then run their own branch's setup (migrations) on top.

// DefaultSnapshot is the snapshot new workspaces start from.
const DefaultSnapshot = "default"

var snapshotName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Snapshot describes a saved snapshot.
type Snapshot struct {
	Name      string    `json:"name"`
	Created   time.Time `json:"created"`
	Workspace string    `json:"workspace"`
	Services  []string  `json:"services,omitempty"`
	// Repo identifies the repository it was saved from; a snapshot is
	// only ever used by that repository.
	Repo string `json:"repo"`
}

func (m *Manager) snapshotDir(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(m.Dirs.State, "snapshots", hex.EncodeToString(sum[:8]))
}

// SaveSnapshot saves a stopped workspace's data disk as name, replacing
// any snapshot of that name.
func (m *Manager) SaveSnapshot(ws registry.Workspace, name string, services []string) error {
	if !snapshotName.MatchString(name) {
		return fmt.Errorf("invalid snapshot name %q: use lowercase letters, digits and dashes", name)
	}
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	// The VM must be stopped, and stay stopped, for a consistent copy.
	unlock, ok, err := fsutil.TryLock(p.lock)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workspace %s is running; stop it first", ws.Name)
	}
	defer unlock()
	if info, err := os.Lstat(p.dataDisk); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("workspace %s has no service data to save", ws.Name)
	}
	repoID, err := repoIdentity(ws.Repo)
	if err != nil {
		return err
	}
	dir := m.snapshotDir(ws.Repo)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// One save at a time per repository: workspaces of one repository
	// saving the same name would otherwise pair one's data with the
	// other's description.
	unlockRepo, err := fsutil.Lock(filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer unlockRepo()
	tmp := filepath.Join(dir, "."+name+".img.tmp")
	_ = os.Remove(tmp)
	if err := clone(p.dataDisk, tmp); err != nil {
		return err
	}
	// The description goes first: an image without one is never used.
	meta, _ := json.MarshalIndent(Snapshot{Name: name, Created: time.Now().UTC(), Workspace: ws.Name, Services: services, Repo: repoID}, "", "  ")
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, name+".json"), meta, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name+".img")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// HasSnapshot reports whether the repository has a snapshot called name.
func (m *Manager) HasSnapshot(repo, name string) bool {
	if !snapshotName.MatchString(name) {
		return false
	}
	_, err := os.Lstat(filepath.Join(m.snapshotDir(repo), name+".img"))
	return err == nil
}

// Snapshots lists a repository's snapshots.
func (m *Manager) Snapshots(repo string) ([]Snapshot, error) {
	dir := m.snapshotDir(repo)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []Snapshot
	for _, path := range paths {
		var s Snapshot
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &s) != nil || !snapshotName.MatchString(s.Name) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, s.Name+".img")); err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveSnapshot deletes one of a repository's snapshots. Workspaces made
// from it keep their data.
func (m *Manager) RemoveSnapshot(repo, name string) error {
	if !snapshotName.MatchString(name) {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	dir := m.snapshotDir(repo)
	err := os.Remove(filepath.Join(dir, name+".img"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no snapshot named %s", name)
	}
	_ = os.Remove(filepath.Join(dir, name+".json"))
	return err
}

// cloneSnapshot clones the repository's snapshot name to dst, reporting
// false if there is no such snapshot.
func (m *Manager) cloneSnapshot(ws registry.Workspace, name, dst string) (bool, error) {
	if !snapshotName.MatchString(name) {
		return false, fmt.Errorf("invalid snapshot name %q", name)
	}
	dir := m.snapshotDir(ws.Repo)
	src := filepath.Join(dir, name+".img")
	info, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		if name == DefaultSnapshot {
			return false, nil
		}
		return false, fmt.Errorf("no snapshot named %s", name)
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", src)
	}
	// The snapshot directory is keyed by the repository's path; make sure
	// it is the same repository, not another one cloned there since.
	var meta Snapshot
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err == nil {
		err = json.Unmarshal(data, &meta)
	}
	repoID, idErr := repoIdentity(ws.Repo)
	if err != nil || idErr != nil || meta.Repo == "" || meta.Repo != repoID {
		if name == DefaultSnapshot {
			fmt.Fprintf(os.Stderr, "not using snapshot %s: it can't be shown to come from the repository now at %s; save it again\n", name, ws.Repo)
			return false, nil
		}
		return false, fmt.Errorf("snapshot %s can't be shown to come from the repository now at %s; save it again from one of its workspaces", name, ws.Repo)
	}
	return true, clone(src, dst)
}
