// Package vmm runs one VM per skerry workspace.
//
// Each running workspace has exactly one runner process (`skerry vm run`)
// that owns the VM and exposes the guest agent on a private Unix socket.
// The runner holds a per-workspace lock for its whole life: a second runner
// exits at once, only the lock holder ever touches the socket, and "is the
// lock held" is the definition of "is the workspace running".
package vmm

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
	"github.com/skerrysoftware-rgb/skerry/internal/assets"
	"github.com/skerrysoftware-rgb/skerry/internal/engine"
	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/image"
	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

// Packages every workspace image gets in this release.
var Packages = []string{"nodejs", "npm"}

// Resources and limits for each workspace VM.
const (
	CPUs      = 4
	MemoryMiB = 3072
	// consoleCap bounds the guest console log (twice this, with rotation).
	consoleCap = 8 << 20
	// runnerLogCap bounds the runner's own log.
	runnerLogCap = 1 << 20
)

// Timeouts for starting and stopping.
const (
	bootTimeout     = 60 * time.Second
	poweroffGrace   = 10 * time.Second
	runnerStopGrace = 20 * time.Second
)

// Manager starts, stops and connects to workspace VMs.
type Manager struct {
	Dirs paths.Dirs
	// Self is the skerry executable that runners are started from.
	Self string
	// Progress receives human-readable progress lines; nil discards them.
	Progress io.Writer
}

func (m *Manager) progressf(format string, args ...any) {
	if m.Progress != nil {
		fmt.Fprintf(m.Progress, format+"\n", args...)
	}
}

// Workspace paths.
type wsPaths struct {
	state, share, socket, lock, console, runnerLog, pid, machineID string
}

func (m *Manager) paths(ws registry.Workspace) (wsPaths, error) {
	sockDir, err := socketDir()
	if err != nil {
		return wsPaths{}, err
	}
	state := m.StateDir(ws)
	return wsPaths{
		state:     state,
		share:     filepath.Join(state, "share"),
		socket:    filepath.Join(sockDir, ws.ID+".sock"),
		lock:      filepath.Join(state, "runner.lock"),
		console:   filepath.Join(state, "console.log"),
		runnerLog: filepath.Join(state, "runner.log"),
		pid:       filepath.Join(state, "runner.pid"),
		machineID: filepath.Join(state, "machine-id"),
	}, nil
}

// StateDir is where a workspace's keys, logs and VM identity live.
func (m *Manager) StateDir(ws registry.Workspace) string {
	return filepath.Join(m.Dirs.State, "workspaces", ws.ID)
}

// socketDir is a short, private directory for runner sockets. Unix socket
// paths are limited to about 104 bytes on macOS, too short for paths under
// Application Support.
func socketDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "skerry-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s must be a directory owned by you with mode 700", dir)
	}
	return dir, nil
}

// Image returns the workspace image, building and finalizing it if needed.
func (m *Manager) Image(ctx context.Context) (image.Image, error) {
	agentBin, err := assets.Agent()
	if err != nil {
		return image.Image{}, err
	}
	keys, err := image.TrustedKeys()
	if err != nil {
		return image.Image{}, err
	}
	b := &image.Builder{
		Dir: m.Dirs.Cache, Keys: keys, Arch: alpineArch(),
		Log: func(f string, a ...any) { m.progressf("  "+f, a...) },
	}
	if err := os.MkdirAll(filepath.Join(m.Dirs.Cache, "images"), 0o755); err != nil {
		return image.Image{}, err
	}
	spec := image.Spec{Packages: Packages, Agent: agentBin}
	img, err := b.Build(ctx, spec)
	if err != nil {
		return image.Image{}, err
	}
	if img.Finalized {
		return img, nil
	}
	// One finalize per image, even with several workspaces starting at once.
	unlock, err := fsutil.Lock(img.Dir + ".lock")
	if err != nil {
		return image.Image{}, err
	}
	defer unlock()
	if img, err = b.Build(ctx, spec); err != nil || img.Finalized {
		return img, err
	}
	m.progressf("  running package install scripts in a build VM")
	if err := m.finalize(ctx, img); err != nil {
		return image.Image{}, err
	}
	return img, b.MarkFinalized(img)
}

func alpineArch() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return "aarch64"
}

// finalize boots the image with its rootfs writable so the packages'
// install scripts can run, then waits for the guest to power itself off.
func (m *Manager) finalize(ctx context.Context, img image.Image) error {
	share, err := os.MkdirTemp(m.Dirs.State, ".finalize-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(share)
	if err := agent.WriteConfig(share, agent.Config{Mode: agent.ModeFinalize, Hostname: "skerry-build"}); err != nil {
		return err
	}
	logPath := img.Dir + ".finalize.log"
	log, err := fsutil.OpenCappedLog(logPath, consoleCap)
	if err != nil {
		return err
	}
	defer log.Close()
	vm, err := engine.Start(engine.Config{
		Kernel: img.Kernel, Initramfs: img.Initramfs, Rootfs: img.Rootfs, RootfsWritable: true,
		ConfigDir: share, CPUs: 2, MemoryMiB: 1024,
		MachineIDFile: filepath.Join(share, "machine-id"), Console: log,
	})
	if err != nil {
		return err
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Minute):
		_ = vm.Stop()
		return fmt.Errorf("build VM did not finish; see %s", logPath)
	case <-ctx.Done():
		_ = vm.Stop()
		return ctx.Err()
	}
	// The guest wrote this marker; accept only a plain file.
	if info, err := os.Lstat(filepath.Join(img.Rootfs, ".skerry", "finalized")); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("install scripts did not complete; see %s", logPath)
	}
	return nil
}

var hostnameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

func hostname(name string) string {
	h := strings.Trim(hostnameUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(h) > 63 {
		h = strings.Trim(h[:63], "-")
	}
	if h == "" {
		h = "workspace"
	}
	return h
}

// keys loads (or on first use creates) the workspace's state dirs and keys.
func (m *Manager) keys(ws registry.Workspace) (wsPaths, agent.Keys, error) {
	p, err := m.paths(ws)
	if err != nil {
		return p, agent.Keys{}, err
	}
	for _, d := range []string{p.state, p.share} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return p, agent.Keys{}, err
		}
	}
	keys, err := agent.EnsureKeys(p.state, p.share)
	return p, keys, err
}

// Run is the runner: it boots the workspace VM, serves the agent socket, and
// returns when the VM stops. Cancelling ctx powers the guest off. A runner
// started while another holds the workspace exits at once with no error.
func (m *Manager) Run(ctx context.Context, ws registry.Workspace) error {
	if ws.Mode != registry.ModeSkerry {
		return fmt.Errorf("%s is a plain worktree; skerry is not enabled for %s", ws.Name, ws.Repo)
	}
	p, keys, err := m.keys(ws)
	if err != nil {
		return err
	}
	unlock, ok, err := fsutil.TryLock(p.lock)
	if err != nil {
		return err
	}
	if !ok {
		return nil // another runner owns this workspace
	}
	defer unlock()

	img, err := m.Image(ctx)
	if err != nil {
		return err
	}
	// Written once, before boot, while nothing reads it.
	cfg := agent.Config{Mode: agent.ModeServe, WorkspacePath: ws.Path, Hostname: hostname(ws.Name)}
	if err := agent.WriteConfig(p.share, cfg); err != nil {
		return err
	}
	console, err := fsutil.OpenCappedLog(p.console, consoleCap)
	if err != nil {
		return err
	}
	defer console.Close()

	vm, err := engine.Start(engine.Config{
		Kernel: img.Kernel, Initramfs: img.Initramfs, Rootfs: img.Rootfs,
		ConfigDir: p.share, WorkDir: ws.Path, CPUs: CPUs, MemoryMiB: MemoryMiB,
		MachineIDFile: p.machineID, Console: console,
	})
	if err != nil {
		return err
	}
	// Only the lock holder gets here, so a socket left at this path is stale.
	_ = os.Remove(p.socket)
	l, err := net.Listen("unix", p.socket)
	if err != nil {
		_ = vm.Stop()
		return err
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	defer os.Remove(p.socket) // runs before unlock: we still own the path
	if err := fsutil.WriteFileAtomic(p.pid, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_ = vm.Stop()
		l.Close()
		return err
	}
	defer os.Remove(p.pid)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go proxy(conn, vm)
		}
	}()

	select {
	case <-vm.Done():
		l.Close()
		return vm.Err()
	case <-ctx.Done():
	}
	l.Close()
	// Ask the guest to shut down cleanly, talking to it directly; force it
	// off if it doesn't (a hostile guest may ignore the request).
	if conn, err := vm.Dial(agent.Port); err == nil {
		if c, err := agent.NewClient(conn, keys); err == nil {
			_ = c.Poweroff()
			c.Close()
		}
	}
	select {
	case <-vm.Done():
	case <-time.After(poweroffGrace):
		_ = vm.Stop()
		<-vm.Done()
	}
	return nil
}

// proxy joins a socket client to the agent, waiting for the agent to come
// up if the VM is still booting.
func proxy(client net.Conn, vm *engine.VM) {
	defer client.Close()
	var guest net.Conn
	deadline := time.Now().Add(bootTimeout)
	for {
		c, err := vm.Dial(agent.Port)
		if err == nil {
			guest = c
			break
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-vm.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer guest.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(guest, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, guest); done <- struct{}{} }()
	<-done
}

// Running reports whether the workspace has a runner.
func (m *Manager) Running(ws registry.Workspace) bool {
	p, err := m.paths(ws)
	if err != nil {
		return false
	}
	if _, err := os.Stat(p.lock); err != nil {
		return false
	}
	unlock, ok, err := fsutil.TryLock(p.lock)
	if err != nil {
		return false
	}
	if ok {
		unlock()
		return false
	}
	return true
}

// Connect returns a client for a running workspace.
func (m *Manager) Connect(ws registry.Workspace) (*agent.Client, error) {
	p, keys, err := m.keys(ws)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", p.socket, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return agent.NewClient(conn, keys)
}

// Up starts the workspace's VM in the background, if it isn't running, and
// waits until its agent answers.
func (m *Manager) Up(ctx context.Context, ws registry.Workspace) (*agent.Client, error) {
	if ws.Mode != registry.ModeSkerry {
		return nil, fmt.Errorf("%s is a plain worktree; to use skerry here, run: skerry enable %s", ws.Name, ws.Repo)
	}
	if c, err := m.Connect(ws); err == nil {
		if c.Ping() == nil {
			return c, nil
		}
		c.Close()
	}
	p, _, err := m.keys(ws)
	if err != nil {
		return nil, err
	}

	var runner *exec.Cmd
	exited := make(chan error, 1)
	if !m.Running(ws) {
		// Build the image here, so progress and errors are visible.
		if _, err := m.Image(ctx); err != nil {
			return nil, err
		}
		m.progressf("starting workspace %s", ws.Name)
		if runner, err = m.spawnRunner(ws, p); err != nil {
			return nil, err
		}
		go func() { exited <- runner.Wait() }()
	}
	// stop ends the runner this call started, if starting failed.
	stop := func() {
		if runner != nil && runner.ProcessState == nil {
			_ = syscall.Kill(-runner.Process.Pid, syscall.SIGTERM)
		}
	}

	deadline := time.After(bootTimeout)
	for {
		if c, err := m.Connect(ws); err == nil {
			if c.Ping() == nil {
				return c, nil
			}
			c.Close()
		}
		select {
		case err := <-exited:
			runner = nil
			if !m.Running(ws) {
				// No runner holds the workspace: ours failed for real.
				return nil, fmt.Errorf("workspace VM exited (%v); see %s and %s", err, p.runnerLog, p.console)
			}
			// Another runner won the race; keep waiting for it.
		case <-deadline:
			stop()
			return nil, fmt.Errorf("workspace VM did not answer within %s; see %s", bootTimeout, p.console)
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *Manager) spawnRunner(ws registry.Workspace, p wsPaths) (*exec.Cmd, error) {
	// Keep the runner log bounded: start over once it grows past its cap.
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if info, err := os.Stat(p.runnerLog); err == nil && info.Size() > runnerLogCap {
		flags |= os.O_TRUNC
	}
	logf, err := os.OpenFile(p.runnerLog, flags, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	runner := exec.Command(m.Self, "vm", "run", ws.ID)
	runner.Stdout, runner.Stderr = logf, logf
	runner.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := runner.Start(); err != nil {
		return nil, err
	}
	return runner, nil
}

// Down stops the workspace's VM and waits for its runner to exit. It asks
// the guest to power off; if the guest ignores that or can't be reached, it
// signals the runner, which forces the VM off.
func (m *Manager) Down(ws registry.Workspace) error {
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	if !m.Running(ws) {
		return nil
	}
	if c, err := m.Connect(ws); err == nil {
		_ = c.Poweroff()
		c.Close()
		if m.waitStopped(ws, poweroffGrace) {
			return nil
		}
	}
	pid, err := m.runnerPID(p, ws)
	if err != nil {
		return fmt.Errorf("workspace %s is running but its runner can't be found (%v); see %s", ws.Name, err, p.runnerLog)
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if m.waitStopped(ws, runnerStopGrace) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // the VM dies with its runner
	if m.waitStopped(ws, 5*time.Second) {
		return nil
	}
	return fmt.Errorf("workspace %s did not stop; see %s", ws.Name, p.runnerLog)
}

func (m *Manager) waitStopped(ws registry.Workspace, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.Running(ws) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !m.Running(ws)
}

// runnerPID reads the runner's pid and checks that the process really is
// this workspace's runner before anything signals it.
func (m *Manager) runnerPID(p wsPaths, ws registry.Workspace) (int, error) {
	data, err := os.ReadFile(p.pid)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("bad pid file %s", p.pid)
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !strings.Contains(string(out), " vm run "+ws.ID) {
		return 0, fmt.Errorf("process %d is not this workspace's runner", pid)
	}
	return pid, nil
}

// Forget deletes a stopped workspace's VM state: keys, logs and identity.
func (m *Manager) Forget(ws registry.Workspace) error {
	if m.Running(ws) {
		return fmt.Errorf("workspace %s is still running", ws.Name)
	}
	if p, err := m.paths(ws); err == nil {
		_ = os.Remove(p.socket)
	}
	return os.RemoveAll(m.StateDir(ws))
}
