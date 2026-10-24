package vmm

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/termsafe"
)

// Preparing a workspace (building its image, booting, starting services,
// installing dependencies) runs in the background, started when a session
// opens, and can take minutes the first time. Nothing waits on it but the
// commands that need the VM; everyone else can see where it is: the status
// line, skerry status, and any command that is waiting, which prints each
// step as it happens.

// Phases of preparing a workspace.
const (
	PhaseImage       = "building the VM image"
	PhaseBoot        = "starting the VM"
	PhaseServices    = "starting services"
	PhaseSetup       = "setting up"
	PhaseReady       = "ready"
	PhaseFailed      = "setup failed"
	PhaseStartFailed = "failed to start"
	PhaseInterrupted = "setup interrupted"
)

// progressMaxAge is how long a phase that isn't updated counts as current.
const progressMaxAge = time.Hour

// Progress is where preparing a workspace has got to.
type Progress struct {
	Phase   string    `json:"phase"`
	Detail  string    `json:"detail,omitempty"`
	PID     int       `json:"pid"`
	Updated time.Time `json:"updated"`
}

// Busy reports whether the workspace is being prepared now.
func (p Progress) Busy() bool {
	switch p.Phase {
	case "", PhaseReady, PhaseFailed, PhaseStartFailed, PhaseInterrupted:
		return false
	}
	return true
}

// Failed reports whether preparing the workspace last ended in failure.
func (p Progress) Failed() bool { return p.Phase == PhaseFailed || p.Phase == PhaseStartFailed }

func (p Progress) String() string {
	if p.Detail == "" {
		return p.Phase
	}
	return p.Phase + ": " + p.Detail
}

// track makes progressf also record progress for ws.
func (m *Manager) track(ws registry.Workspace) {
	if p, err := m.paths(ws); err == nil {
		m.progressFile = p.progress
	}
}

// phase records a new phase (and prints it, if anyone is watching).
func (m *Manager) phase(phase, detail string) {
	m.writeProgress(Progress{Phase: phase, Detail: detail})
}

func (m *Manager) writeProgress(pr Progress) {
	if m.progressFile == "" {
		return
	}
	pr.PID, pr.Updated = os.Getpid(), time.Now().UTC()
	pr.Detail = termsafe.Clean(pr.Detail)
	if r := []rune(pr.Detail); len(r) > 200 {
		pr.Detail = string(r[:200]) + "…"
	}
	m.lastProgress = pr
	data, _ := json.Marshal(pr)
	_ = fsutil.WriteFileAtomic(m.progressFile, data, 0o600)
}

// ReadProgress returns where preparing ws has got to. A phase left by a
// process that has since died is reported as not busy.
func (m *Manager) ReadProgress(ws registry.Workspace) Progress {
	p, err := m.paths(ws)
	if err != nil {
		return Progress{}
	}
	return readProgress(p.progress)
}

func readProgress(path string) Progress {
	var pr Progress
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &pr) != nil {
		return Progress{}
	}
	if pr.Busy() && (pr.PID <= 0 || syscall.Kill(pr.PID, 0) == syscall.ESRCH || time.Since(pr.Updated) > progressMaxAge) {
		pr.Phase, pr.Detail = "", "" // its process is gone, or it is stale
	}
	return pr
}

// waitLock takes a file lock, waiting until it is free or ctx ends. While
// it waits, it prints each step of the work the holder reports in
// progressPath, so a waiting command says what it is waiting for.
func (m *Manager) waitLock(ctx context.Context, path, progressPath string) (func(), error) {
	var last Progress
	for {
		unlock, ok, err := fsutil.TryLock(path)
		if err != nil || ok {
			return unlock, err
		}
		if pr := readProgress(progressPath); pr.Busy() && (pr.Phase != last.Phase || pr.Detail != last.Detail) {
			m.progressf("waiting for the workspace: %s", pr)
			last = pr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
