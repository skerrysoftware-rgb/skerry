// Package vmm runs one VM per skerry workspace.
//
// Each running workspace has a runner process (`skerry vm run`) that owns
// the VM and exposes the guest agent on a private Unix socket. Commands such
// as `skerry exec` connect to that socket and speak SSH to the agent.
package vmm

import (
	"context"
	"errors"
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

// Resources for each workspace VM.
const (
	CPUs      = 4
	MemoryMiB = 3072
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
	state, share, socket, console, runnerLog, pid, machineID string
}

func (m *Manager) paths(ws registry.Workspace) (wsPaths, error) {
	sockDir, err := socketDir()
	if err != nil {
		return wsPaths{}, err
	}
	state := filepath.Join(m.Dirs.State, "workspaces", ws.ID)
	return wsPaths{
		state:     state,
		share:     filepath.Join(state, "share"),
		socket:    filepath.Join(sockDir, ws.ID+".sock"),
		console:   filepath.Join(state, "console.log"),
		runnerLog: filepath.Join(state, "runner.log"),
		pid:       filepath.Join(state, "runner.pid"),
		machineID: filepath.Join(state, "machine-id"),
	}, nil
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
	img, err := b.Build(ctx, image.Spec{Packages: Packages, Agent: agentBin})
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
	if img, err = b.Build(ctx, image.Spec{Packages: Packages, Agent: agentBin}); err != nil || img.Finalized {
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
	log, err := os.Create(logPath)
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
	if _, err := os.Stat(filepath.Join(img.Rootfs, ".skerry", "finalized")); err != nil {
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

// prepare creates the workspace's state directory, keys and agent config.
func (m *Manager) prepare(ws registry.Workspace) (wsPaths, agent.Keys, error) {
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
	if err != nil {
		return p, agent.Keys{}, err
	}
	err = agent.WriteConfig(p.share, agent.Config{Mode: agent.ModeServe, WorkspacePath: ws.Path, Hostname: hostname(ws.Name)})
	return p, keys, err
}

// Run is the runner: it boots the workspace VM, serves the agent socket, and
// returns when the VM stops. Cancelling ctx powers the guest off.
func (m *Manager) Run(ctx context.Context, ws registry.Workspace) error {
	if ws.Mode != registry.ModeSkerry {
		return fmt.Errorf("%s is a plain worktree; skerry is not enabled for %s", ws.Name, ws.Repo)
	}
	img, err := m.Image(ctx)
	if err != nil {
		return err
	}
	p, keys, err := m.prepare(ws)
	if err != nil {
		return err
	}
	console, err := os.OpenFile(p.console, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
	_ = os.Remove(p.socket)
	l, err := net.Listen("unix", p.socket)
	if err != nil {
		_ = vm.Stop()
		return err
	}
	defer os.Remove(p.socket)
	if err := os.WriteFile(p.pid, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_ = vm.Stop()
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
	// Ask the guest to shut down cleanly; force it after a grace period.
	if c, err := m.dialAgent(p, keys); err == nil {
		_ = c.Poweroff()
		c.Close()
	}
	select {
	case <-vm.Done():
	case <-time.After(15 * time.Second):
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
	deadline := time.Now().Add(60 * time.Second)
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

func (m *Manager) dialAgent(p wsPaths, keys agent.Keys) (*agent.Client, error) {
	conn, err := net.DialTimeout("unix", p.socket, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return agent.NewClient(conn, keys)
}

// Connect returns a client for a running workspace.
func (m *Manager) Connect(ws registry.Workspace) (*agent.Client, error) {
	p, keys, err := m.prepare(ws)
	if err != nil {
		return nil, err
	}
	return m.dialAgent(p, keys)
}

// Running reports whether the workspace's agent answers.
func (m *Manager) Running(ws registry.Workspace) bool {
	p, err := m.paths(ws)
	if err != nil {
		return false
	}
	if _, err := os.Stat(p.socket); err != nil {
		return false
	}
	c, err := m.Connect(ws)
	if err != nil {
		return false
	}
	defer c.Close()
	return c.Ping() == nil
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
	// Build the image in the foreground, so progress and errors are visible.
	if _, err := m.Image(ctx); err != nil {
		return nil, err
	}
	p, _, err := m.prepare(ws)
	if err != nil {
		return nil, err
	}
	m.progressf("starting workspace %s", ws.Name)
	logf, err := os.OpenFile(p.runnerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	runner := exec.Command(m.Self, "vm", "run", ws.ID)
	runner.Stdout, runner.Stderr = logf, logf
	runner.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := runner.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	logf.Close()
	exited := make(chan error, 1)
	go func() { exited <- runner.Wait() }()

	deadline := time.After(60 * time.Second)
	for {
		if c, err := m.Connect(ws); err == nil {
			if c.Ping() == nil {
				return c, nil
			}
			c.Close()
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("workspace VM exited (%v); see %s and %s", err, p.runnerLog, p.console)
		case <-deadline:
			return nil, fmt.Errorf("workspace VM did not answer within 60s; see %s", p.console)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Down powers the workspace's VM off and waits for its runner to exit.
func (m *Manager) Down(ws registry.Workspace) error {
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	c, err := m.Connect(ws)
	if err != nil {
		return nil // not running
	}
	err = c.Poweroff()
	c.Close()
	if err != nil {
		return err
	}
	for range 150 {
		if _, err := os.Stat(p.socket); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("workspace %s did not stop; see %s", ws.Name, p.runnerLog)
}
