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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
	"github.com/skerrysoftware-rgb/skerry/internal/assets"
	"github.com/skerrysoftware-rgb/skerry/internal/engine"
	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/guard"
	"github.com/skerrysoftware-rgb/skerry/internal/image"
	"github.com/skerrysoftware-rgb/skerry/internal/netx"
	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/services"
	"github.com/skerrysoftware-rgb/skerry/internal/termsafe"
)

// Packages every workspace image gets in this release; services add theirs.
var Packages = []string{"nodejs-24", "npm", "curl"}

// Resources and limits for each workspace VM.
const (
	CPUs      = 4
	MemoryMiB = 3072
	// consoleCap bounds the guest console log (twice this, with rotation).
	consoleCap = 8 << 20
	// runnerLogCap bounds the runner's own log.
	runnerLogCap = 1 << 20
	// networkLogCap bounds the log of refused connections, which the
	// guest controls.
	networkLogCap = 1 << 20
	// dataDiskSize is the data disk's capacity. The file is sparse: it
	// uses only what the guest writes.
	dataDiskSize = 64 << 30
	// servicesTimeout bounds waiting for services at startup; the first
	// boot initializes their data.
	servicesTimeout = 3 * time.Minute
)

// Timeouts for starting and stopping.
const (
	bootTimeout = 60 * time.Second
	// poweroffGrace covers the guest stopping its services cleanly.
	poweroffGrace = 20 * time.Second
	// runnerStopGrace gives a runner time to power its VM off (up to
	// poweroffGrace) and then force it off.
	runnerStopGrace = poweroffGrace + 15*time.Second
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
	dataDisk, secret, status, networkLog, image                    string
}

func (m *Manager) paths(ws registry.Workspace) (wsPaths, error) {
	sockDir, err := socketDir()
	if err != nil {
		return wsPaths{}, err
	}
	state := m.StateDir(ws)
	return wsPaths{
		state:      state,
		share:      filepath.Join(state, "share"),
		socket:     filepath.Join(sockDir, ws.ID+".sock"),
		lock:       filepath.Join(state, "runner.lock"),
		console:    filepath.Join(state, "console.log"),
		runnerLog:  filepath.Join(state, "runner.log"),
		pid:        filepath.Join(state, "runner.pid"),
		machineID:  filepath.Join(state, "machine-id"),
		dataDisk:   filepath.Join(state, "data.img"),
		secret:     filepath.Join(state, "secret"),
		status:     filepath.Join(state, "status.json"),
		networkLog: filepath.Join(state, "network.log"),
		image:      filepath.Join(state, "image"),
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

// Image returns the image for a project, building and finalizing it if
// needed.
func (m *Manager) Image(ctx context.Context, proj project.Project) (image.Image, error) {
	agentBin, err := assets.Agent()
	if err != nil {
		return image.Image{}, err
	}
	b, err := m.builder()
	if err != nil {
		return image.Image{}, err
	}
	if err := os.MkdirAll(filepath.Join(m.Dirs.Cache, "images"), 0o755); err != nil {
		return image.Image{}, err
	}
	spec := image.Spec{Packages: append(slices.Clone(Packages), services.Packages(proj.Services)...), Agent: agentBin}
	// A pinned image is used as it is; otherwise the newest packages are
	// resolved, and the result (perhaps the same image) is pinned afresh.
	img, pinned := b.Pinned(spec, imageMaxAge)
	if !pinned {
		if img, err = b.Build(ctx, spec); err != nil {
			return image.Image{}, err
		}
	}
	if img.Finalized {
		if pinned {
			return img, nil
		}
		return img, b.Pin(spec, img)
	}
	// One finalize per image, even with several workspaces starting at once.
	unlock, err := fsutil.Lock(img.Dir + ".lock")
	if err != nil {
		return image.Image{}, err
	}
	defer unlock()
	if img, err = b.Build(ctx, spec); err != nil {
		return img, err
	}
	if img.Finalized {
		return img, b.Pin(spec, img)
	}
	m.progressf("  running package install scripts in a build VM")
	if err := m.finalize(ctx, img); err != nil {
		return image.Image{}, err
	}
	if err := b.MarkFinalized(img); err != nil {
		return image.Image{}, err
	}
	img.Finalized = true
	return img, b.Pin(spec, img)
}

// imageMaxAge is how long a project keeps the image built for it before
// skerry rebuilds it with the newest packages (security fixes included).
const imageMaxAge = 7 * 24 * time.Hour

// builder is the image builder for this machine.
func (m *Manager) builder() (*image.Builder, error) {
	system, err := image.Wolfi()
	if err != nil {
		return nil, err
	}
	kernel, err := image.Alpine()
	if err != nil {
		return nil, err
	}
	return &image.Builder{
		Dir: m.Dirs.Cache, System: system, Kernel: kernel, Arch: image.WolfiArch(runtime.GOARCH),
		Log: func(f string, a ...any) { m.progressf("  "+f, a...) },
	}, nil
}

// Collect deletes images and downloads no workspace needs: images not
// pinned for any project and not in use by a running workspace.
func (m *Manager) Collect(all []registry.Workspace) ([]string, error) {
	b, err := m.builder()
	if err != nil {
		return nil, err
	}
	inUse := map[string]bool{}
	for _, ws := range all {
		if !m.Running(ws) {
			continue
		}
		p, err := m.paths(ws)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(p.image)
		if err != nil {
			// A running workspace whose image isn't known: keep everything.
			return nil, fmt.Errorf("workspace %s is running an unknown image; stop it and retry", ws.Name)
		}
		inUse[strings.TrimSpace(string(data))] = true
	}
	return b.Collect(inUse, imageMaxAge)
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
	// Running() takes the lock for a moment to test it, so retry briefly
	// before concluding another runner owns the workspace.
	var unlock func()
	ok := false
	for attempt := 0; attempt < 10 && !ok; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if unlock, ok, err = fsutil.TryLock(p.lock); err != nil {
			return err
		}
	}
	if !ok {
		return nil // another runner owns this workspace
	}
	defer unlock()

	proj, err := project.Load(ws.Path)
	if err != nil {
		return err
	}
	img, err := m.Image(ctx, proj)
	if err != nil {
		return err
	}
	if err := guard.Protect(ws); err != nil {
		return fmt.Errorf("protect %s/.git: %w", ws.Path, err)
	}
	// So skerry gc never deletes the image under a running VM.
	if err := fsutil.WriteFileAtomic(p.image, []byte(img.ID+"\n"), 0o600); err != nil {
		return err
	}
	// Written once, before boot, while nothing reads it.
	cfg := agent.Config{
		Mode: agent.ModeServe, WorkspacePath: ws.Path, Hostname: hostname(ws.Name),
		Network: &agent.Network{
			Address: netip.PrefixFrom(netx.GuestIP, netx.PrefixLen),
			Gateway: netx.GatewayIP, DNS: netx.GatewayIP,
		},
	}
	dataDisk := ""
	if len(proj.Services) > 0 {
		if cfg.Secret, err = secret(p.secret); err != nil {
			return err
		}
		if err := ensureDisk(p.dataDisk, dataDiskSize); err != nil {
			return err
		}
		cfg.Services, cfg.DataDisk, dataDisk = proj.Services, true, p.dataDisk
	}
	if err := agent.WriteConfig(p.share, cfg); err != nil {
		return err
	}
	console, err := fsutil.OpenCappedLog(p.console, consoleCap)
	if err != nil {
		return err
	}
	defer console.Close()

	// Opened first so it is closed last, after the network stops logging.
	netLog, err := fsutil.OpenCappedLog(p.networkLog, networkLogCap)
	if err != nil {
		return err
	}
	defer netLog.Close()
	network, err := netx.New(netx.DefaultPolicy)
	if err != nil {
		return err
	}
	defer network.Close()
	network.Log = log.New(netLog, "", log.LstdFlags)
	vmNet, hostNet, err := netx.FramePair()
	if err != nil {
		return err
	}
	netCtx, stopNet := context.WithCancel(context.Background())
	defer stopNet()
	network.Attach(netCtx, hostNet)

	vm, err := engine.Start(engine.Config{
		Kernel: img.Kernel, Initramfs: img.Initramfs, Rootfs: img.Rootfs,
		ConfigDir: p.share, WorkDir: ws.Path, CPUs: CPUs, MemoryMiB: MemoryMiB,
		MachineIDFile: p.machineID, Console: console,
		Network: vmNet, MAC: macFor(ws.ID), DataDisk: dataDisk,
	})
	if err != nil {
		vmNet.Close()
		return err
	}
	dialer := &guestDialer{vm: vm, keys: keys, open: map[net.Conn]bool{}}
	defer dialer.close()
	forwards, closeForwards := m.forward(dialer, p, proj)
	defer closeForwards()
	if err := writeStatus(p.status, Status{Hostname: hostname(ws.Name), Services: proj.Services, Source: proj.Source, Forwards: forwards}); err != nil {
		fmt.Fprintf(os.Stderr, "write status: %v\n", err)
	}
	defer os.Remove(p.status)
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
	// Code in the VM keeps running between commands (dev servers, watchers),
	// so check for planted files on a timer too.
	scanCtx, stopScan := context.WithCancel(ctx)
	defer stopScan()
	go watchCheckout(scanCtx, ws)

	select {
	case <-vm.Done():
		l.Close()
		return vm.Err()
	case <-ctx.Done():
	}
	l.Close()
	// Ask the guest to shut down cleanly, talking to it directly; force it
	// off if it doesn't (a hostile guest may ignore the request, or stall
	// the connection, so asking gets a few seconds at most).
	asked := make(chan struct{})
	go func() {
		defer close(asked)
		if conn, err := vm.Dial(agent.Port); err == nil {
			if c, err := agent.NewClient(conn, keys); err == nil {
				_ = c.Poweroff()
				c.Close()
			}
		}
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
	}
	select {
	case <-vm.Done():
	case <-time.After(poweroffGrace):
		_ = vm.Stop()
		select {
		case <-vm.Done():
		case <-time.After(10 * time.Second):
			// The framework didn't stop it; exiting ends the VM with us.
			return errors.New("the VM did not stop")
		}
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
// waits until its agent answers and its services are ready.
func (m *Manager) Up(ctx context.Context, ws registry.Workspace) (*agent.Client, error) {
	c, err := m.boot(ctx, ws)
	if err != nil {
		return nil, err
	}
	// A command may not need the services, so a failed service is a
	// warning, not a reason to refuse to run anything.
	if err := m.waitServices(ctx, c); err != nil {
		if ctx.Err() != nil {
			c.Close()
			return nil, ctx.Err()
		}
		m.progressf("WARNING: %s", termsafe.Clean(err.Error()))
	}
	return c, nil
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
// It is safe for concurrent writers (a command's stdout and stderr).
type limitedBuffer struct {
	max int
	mu  sync.Mutex
	b   strings.Builder
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitServices blocks until the guest reports every service ready. It
// costs one round trip when they already are.
func (m *Manager) waitServices(ctx context.Context, c *agent.Client) error {
	slow := time.AfterFunc(time.Second, func() { m.progressf("waiting for services to start") })
	defer slow.Stop()
	// The guest decides when this ends and what it prints, so bound both.
	ctx, cancel := context.WithTimeout(ctx, servicesTimeout+30*time.Second)
	defer cancel()
	out := &limitedBuffer{max: 4 << 10}
	code, err := c.Run(ctx, agent.Cmd{
		Args:   []string{"/.skerry/agent", "wait", servicesTimeout.String()},
		Stdout: out, Stderr: out,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("services did not start: %s", strings.TrimSpace(out.String()))
	}
	return nil
}

func (m *Manager) boot(ctx context.Context, ws registry.Workspace) (*agent.Client, error) {
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
	var runnerDone atomic.Bool // set once Wait has returned
	exited := make(chan error, 1)
	if !m.Running(ws) {
		// Build the image here, so progress and errors are visible.
		proj, err := project.Load(ws.Path)
		if err != nil {
			return nil, err
		}
		if _, err := m.Image(ctx, proj); err != nil {
			return nil, err
		}
		m.progressf("starting workspace %s", ws.Name)
		if runner, err = m.spawnRunner(ws, p); err != nil {
			return nil, err
		}
		go func() {
			err := runner.Wait()
			runnerDone.Store(true)
			exited <- err
		}()
	}
	// stop ends the runner this call started, if starting failed.
	stop := func() {
		if runner != nil && !runnerDone.Load() {
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

// ResetData deletes a stopped workspace's data disk; the next start makes
// a blank one.
func (m *Manager) ResetData(ws registry.Workspace) error {
	if m.Running(ws) {
		return fmt.Errorf("workspace %s is still running", ws.Name)
	}
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	// Hold the runner's lock, so nothing can start the VM on this disk
	// while it is deleted.
	unlock, ok, err := fsutil.TryLock(p.lock)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workspace %s started again; stop it and retry", ws.Name)
	}
	defer unlock()
	if err := os.Remove(p.dataDisk); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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

// watchCheckout scans the checkout for planted files every few seconds while
// the VM runs, quarantining what it finds. Findings go to the runner log.
func watchCheckout(ctx context.Context, ws registry.Workspace) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := guard.CheckPointer(ws); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: %s\n", termsafe.Clean(err.Error()))
		}
		found, _ := guard.Scan(ctx, ws)
		for _, f := range found {
			fmt.Fprintf(os.Stderr, "WARNING: found %s\n", termsafe.Clean(f.String()))
		}
	}
}

// Status is what a running workspace exposes, written by its runner.
type Status struct {
	// Hostname names the workspace in .localhost URLs.
	Hostname string    `json:"hostname"`
	Services []string  `json:"services,omitempty"`
	Source   string    `json:"source,omitempty"`
	Forwards []Forward `json:"forwards,omitempty"`
}

// Forward is a guest port reachable on the host's loopback interface.
type Forward struct {
	Name  string `json:"name"`
	Guest uint16 `json:"guest"`
	Host  uint16 `json:"host"`
	// Web says it serves HTTP.
	Web bool `json:"web,omitempty"`
}

func writeStatus(path string, st Status) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// Status reports a running workspace's services and forwards, and the
// password its services use.
func (m *Manager) Status(ws registry.Workspace) (Status, string, error) {
	var st Status
	p, err := m.paths(ws)
	if err != nil {
		return st, "", err
	}
	data, err := os.ReadFile(p.status)
	if err != nil {
		return st, "", err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, "", err
	}
	pw, _ := os.ReadFile(p.secret)
	return st, strings.TrimSpace(string(pw)), nil
}

// forward exposes the services' ports and the project's own on the host's
// loopback interface. Each guest port keeps the host port it had last
// time when that is free, so URLs stay stable across restarts.
func (m *Manager) forward(d *guestDialer, p wsPaths, proj project.Project) ([]Forward, func()) {
	var want []Forward
	known := map[uint16]services.Port{}
	for _, name := range proj.Services {
		preset, _ := services.Lookup(name)
		for _, port := range preset.Ports {
			known[port.Guest] = port
			if port.Forward {
				want = append(want, Forward{Name: port.Name, Guest: port.Guest, Web: port.Web})
			}
		}
	}
	// The project's own ports; they serve the project's apps, so they are
	// treated as web servers unless a service owns them.
	for _, port := range proj.Ports {
		f := Forward{Name: "port " + strconv.Itoa(int(port)), Guest: port, Web: true}
		if k, ok := known[port]; ok {
			f.Name, f.Web = k.Name, k.Web
		}
		want = append(want, f)
	}
	previous := map[uint16]uint16{}
	if data, err := os.ReadFile(p.status + ".ports"); err == nil {
		_ = json.Unmarshal(data, &previous)
	}
	var (
		got       []Forward
		listeners []net.Listener
		seen      = map[uint16]bool{}
	)
	for _, f := range want {
		if seen[f.Guest] {
			continue
		}
		seen[f.Guest] = true
		l, err := d.listen(previous[f.Guest], f.Guest)
		if err != nil && previous[f.Guest] != 0 {
			l, err = d.listen(0, f.Guest)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "forward %s: %v\n", f.Name, err)
			continue
		}
		f.Host = uint16(l.Addr().(*net.TCPAddr).Port)
		previous[f.Guest] = f.Host
		got = append(got, f)
		listeners = append(listeners, l)
	}
	if data, err := json.Marshal(previous); err == nil {
		_ = fsutil.WriteFileAtomic(p.status+".ports", data, 0o600)
	}
	return got, func() {
		for _, l := range listeners {
			l.Close()
		}
	}
}

// secret returns the workspace's service password, creating it once.
func secret(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(data))
		if len(s) == 64 {
			if _, err := hex.DecodeString(s); err == nil {
				return s, nil
			}
		}
		return "", fmt.Errorf("%s is corrupt; delete it to make a new password", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := hex.EncodeToString(b)
	return s, fsutil.WriteFileAtomic(path, []byte(s+"\n"), 0o600)
}

// ensureDisk creates a sparse disk image of size bytes if none exists.
func ensureDisk(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// macFor derives a stable, locally administered MAC address for a
// workspace.
func macFor(id string) net.HardwareAddr {
	sum := sha256.Sum256([]byte("skerry-mac:" + id))
	mac := net.HardwareAddr(sum[:6])
	mac[0] = mac[0]&0xfc | 0x02 // unicast, locally administered
	return mac
}
