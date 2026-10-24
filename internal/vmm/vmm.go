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

// Packages every workspace image gets; runtimes (Node always among them),
// services and the project's own extra packages add theirs. Package
// managers fetch dependencies pinned to git URLs with git (the checkout's
// git metadata isn't in the VM, so git there sees no repository), and
// compile native modules (node-gyp, Python source packages, Ruby gems)
// with a C and C++ toolchain; build-base also brings make, which
// Procfiles and setup scripts often run.
var Packages = []string{"curl", "git", "build-base"}

// ImagePackages lists the packages a project's image needs, runtimes first
// so that a runtime's version is the one later packages resolve against.
func ImagePackages(proj project.Project) []string {
	pkgs := services.RuntimePackages(proj.Runtimes)
	pkgs = append(pkgs, Packages...)
	pkgs = append(pkgs, services.Packages(proj.Services)...)
	return append(pkgs, proj.Packages...)
}

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
	// homeDiskSize is the home disk's capacity, sparse like the data
	// disk's.
	homeDiskSize = 64 << 30
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

	// progressFile, once Up starts on a workspace, records its progress
	// for other processes to show; lastProgress is what it last recorded.
	progressFile string
	lastProgress Progress
}

func (m *Manager) progressf(format string, args ...any) {
	if m.Progress != nil {
		fmt.Fprintf(m.Progress, format+"\n", args...)
	}
}

// Workspace paths.
type wsPaths struct {
	state, share, socket, lock, console, runnerLog, pid, machineID string
	dataDisk, homeDisk, secret, status, networkLog, image, setup   string
	setupLog                                                       string
	stopping, progress, prepare, runtimes, imageSpec               string
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
		homeDisk:   filepath.Join(state, "home.img"),
		secret:     filepath.Join(state, "secret"),
		status:     filepath.Join(state, "status.json"),
		networkLog: filepath.Join(state, "network.log"),
		image:      filepath.Join(state, "image"),
		setup:      filepath.Join(state, "setup.json"),
		setupLog:   filepath.Join(state, "setup.log"),
		stopping:   filepath.Join(state, "stopping"),
		progress:   filepath.Join(state, "progress.json"),
		prepare:    filepath.Join(state, "prepare.lock"),
		runtimes:   filepath.Join(state, "runtimes"),
		imageSpec:  filepath.Join(state, "image-spec"),
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
	spec := image.Spec{Packages: ImagePackages(proj), Agent: agentBin}
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
		Log: func(f string, a ...any) {
			m.progressf("  "+f, a...)
			if m.lastProgress.Phase == PhaseImage {
				m.writeProgress(Progress{Phase: PhaseImage, Detail: fmt.Sprintf(f, a...)})
			}
		},
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
	_ = fsutil.WriteFileAtomic(p.imageSpec, []byte(imageSpec(proj)+"\n"), 0o600)
	m.noteRuntimes(ws, proj)
	// Written once, before boot, while nothing reads it.
	cfg := agent.Config{
		Mode: agent.ModeServe, WorkspacePath: ws.Path, Hostname: hostname(ws.Name),
		Network: &agent.Network{
			Address: netip.PrefixFrom(netx.GuestIP, netx.PrefixLen),
			Gateway: netx.GatewayIP, DNS: netx.GatewayIP,
		},
	}
	for _, a := range proj.AllApps {
		app := agent.App{Name: a.Name, Dir: filepath.Join(ws.Path, filepath.FromSlash(a.Dir)), Command: a.Command, Ports: a.Ports}
		for _, f := range a.EnvFiles {
			app.EnvFiles = append(app.EnvFiles, filepath.Join(ws.Path, filepath.FromSlash(f)))
		}
		cfg.Apps = append(cfg.Apps, app)
	}
	for _, a := range proj.PortAliases {
		if preset, ok := services.Lookup(a.Service); ok {
			cfg.PortAliases = append(cfg.PortAliases, agent.PortAlias{Port: a.Port, To: preset.Ports[0].Guest})
		}
	}
	dataDisk := ""
	if len(proj.Services) > 0 {
		if cfg.Secret, err = secret(p.secret); err != nil {
			return err
		}
		created, err := m.ensureDisk(ws, p, dataDiskSize)
		if err != nil {
			return err
		}
		if created {
			_ = os.Remove(p.setup) // new data: seed and set up again
		}
		cfg.Services, cfg.DataDisk, dataDisk = proj.Services, true, p.dataDisk
	}
	// The home disk holds what commands keep under the home directory and
	// in /tmp: package managers' caches, downloaded browsers, build
	// output. Without it they would fill the root filesystem, which is in
	// memory. It is the workspace's own: not in snapshots, kept by reset,
	// deleted with the workspace.
	homeDisk := p.homeDisk
	if err := ensureSparse(homeDisk, homeDiskSize); err != nil {
		fmt.Fprintf(os.Stderr, "home disk: %v; caches stay in memory\n", err)
		homeDisk = ""
	}
	cfg.HomeDisk = homeDisk != ""
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

	cpus, memMiB := resources(proj)
	vm, err := engine.Start(engine.Config{
		Kernel: img.Kernel, Initramfs: img.Initramfs, Rootfs: img.Rootfs,
		ConfigDir: p.share, WorkDir: ws.Path, CPUs: cpus, MemoryMiB: memMiB,
		MachineIDFile: p.machineID, Console: console,
		Network: vmNet, MAC: macFor(ws.ID), DataDisk: dataDisk, HomeDisk: homeDisk,
	})
	if err != nil {
		vmNet.Close()
		return err
	}
	act := newActivity()
	dialer := &guestDialer{vm: vm, keys: keys, open: map[net.Conn]bool{}, act: act, own: hostname(ws.Name) + ".localhost"}
	defer dialer.close()
	forwards, closeForwards := m.forward(dialer, p, proj)
	defer closeForwards()
	if w, err := forwardFileEvents(ws, dialer); err != nil {
		fmt.Fprintf(os.Stderr, "file events: %v\n", err)
	} else if w != nil {
		defer w.Close()
	}
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
			go func() {
				defer act.begin()()
				proxy(conn, vm)
			}()
		}
	}()
	// Code in the VM keeps running between commands (dev servers, watchers),
	// so check for planted files on a timer too.
	scanCtx, stopScan := context.WithCancel(ctx)
	defer stopScan()
	go watchCheckout(scanCtx, ws)

	idle := idleStop()
	stopIdle := make(chan struct{})
	defer close(stopIdle)
	select {
	case <-vm.Done():
		l.Close()
		return vm.Err()
	case <-ctx.Done():
	case <-act.whenIdle(idle, stopIdle):
		fmt.Fprintf(os.Stderr, "stopping: idle for %s\n", idle)
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
	m.track(ws)
	c, err := m.boot(ctx, ws)
	if err != nil {
		if m.lastProgress.Busy() {
			m.phase(PhaseStartFailed, err.Error())
		}
		return nil, err
	}
	// A command may not need the services, so a failed service is a
	// warning, not a reason to refuse to run anything.
	if m.lastProgress.Busy() {
		m.phase(PhaseServices, "")
	}
	if err := m.waitServices(ctx, c); err != nil {
		if ctx.Err() != nil {
			c.Close()
			return nil, ctx.Err()
		}
		m.progressf("WARNING: %s", termsafe.Clean(err.Error()))
	}
	proj, err := project.Load(ws.Path)
	if err == nil {
		m.noteRuntimes(ws, proj)
		m.warnStaleImage(ws, proj)
		var ran bool
		ran, err = m.ensureSetup(ctx, c, ws, proj)
		// Apps that are running restart on what setup installed; the
		// others stay stopped until someone starts them.
		if err == nil && ran && len(proj.AllApps) > 0 && !m.setupFailed(ws) {
			err = m.appsCommand(ctx, c, "apps-restart")
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			c.Close()
			return nil, ctx.Err()
		}
		m.progressf("WARNING: %s", termsafe.Clean(err.Error()))
		if m.lastProgress.Busy() {
			m.phase(PhaseInterrupted, "")
		}
	} else if m.lastProgress.Busy() {
		m.phase(PhaseReady, "")
	}
	return c, nil
}

// maxForwards bounds the ports forwarded for one workspace.
const maxForwards = 48

// appsCommand asks the VM's app supervisor to start, stop or restart
// apps.
func (m *Manager) appsCommand(ctx context.Context, c *agent.Client, cmd string, names ...string) error {
	out := &limitedBuffer{max: 4 << 10}
	code, err := c.Run(ctx, agent.Cmd{Args: append([]string{"/.skerry/agent", cmd}, names...), Stdout: out, Stderr: out})
	if err == nil && code != 0 {
		err = fmt.Errorf("%s", strings.TrimSpace(out.String()))
	}
	return err
}

// StartApps starts apps in a running workspace (all of the project's, if
// none are named), with the dev watchers they use, once setup has
// succeeded: apps need what it installs.
func (m *Manager) StartApps(ctx context.Context, ws registry.Workspace, names []string) ([]string, error) {
	proj, err := project.Load(ws.Path)
	if err != nil {
		return nil, err
	}
	if names, err = proj.AppNames(names); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("the project has no apps skerry knows how to run (skerry plan shows what it found)")
	}
	if m.setupFailed(ws) {
		return nil, errors.New("setup failed, so the apps can't start; see skerry logs setup, and rerun it with skerry setup")
	}
	c, err := m.Connect(ws)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return names, m.appsCommand(ctx, c, "apps-start", names...)
}

// StopApps stops apps (all, if none are named), and the dev watchers no
// app still running needs.
func (m *Manager) StopApps(ctx context.Context, ws registry.Workspace, names []string) error {
	if len(names) > 0 {
		proj, err := project.Load(ws.Path)
		if err != nil {
			return err
		}
		states, err := m.AppStates(ctx, ws)
		if err != nil {
			return err
		}
		running := func(name string) bool {
			return !slices.Contains(names, name) && slices.ContainsFunc(states, func(s agent.AppState) bool {
				return s.Name == name && s.State != "stopped"
			})
		}
		for _, w := range proj.AllApps {
			if len(w.For) > 0 && !slices.Contains(names, w.Name) && !slices.ContainsFunc(w.For, running) {
				names = append(names, w.Name)
			}
		}
	}
	c, err := m.Connect(ws)
	if err != nil {
		return err
	}
	defer c.Close()
	return m.appsCommand(ctx, c, "apps-stop", names...)
}

// AppStates asks the VM how the project's apps are doing.
func (m *Manager) AppStates(ctx context.Context, ws registry.Workspace) ([]agent.AppState, error) {
	c, err := m.Connect(ws)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := &limitedBuffer{max: 256 << 10}
	code, err := c.Run(ctx, agent.Cmd{Args: []string{"/.skerry/agent", "apps-status"}, Stdout: out})
	if err != nil || code != 0 {
		return nil, fmt.Errorf("asking the VM for its apps: %v", err)
	}
	var states []agent.AppState
	if err := json.Unmarshal([]byte(out.String()), &states); err != nil {
		return nil, err
	}
	return states, nil
}

// warnStaleImage says when skerry.yaml asks for runtimes, services or
// packages the running VM doesn't have: a VM keeps its image until it
// restarts.
func (m *Manager) warnStaleImage(ws registry.Workspace, proj project.Project) {
	p, err := m.paths(ws)
	if err != nil {
		return
	}
	running, err := os.ReadFile(p.imageSpec)
	if err != nil || strings.TrimSpace(string(running)) == imageSpec(proj) {
		return
	}
	m.progressf("WARNING: skerry.yaml asks for runtimes, services or packages this VM doesn't have; " +
		"they take effect when it restarts (skerry down, then any command)")
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

	bootStarted := time.Now()
	var (
		runner     *exec.Cmd
		runnerDone atomic.Bool // set once Wait has returned
		exited     = make(chan error, 1)
		starts     int
	)
	start := func() error {
		// Build the image here, so progress and errors are visible.
		proj, err := project.Load(ws.Path)
		if err != nil {
			return err
		}
		m.phase(PhaseImage, "")
		if _, err := m.Image(ctx, proj); err != nil {
			return err
		}
		if stoppedSince(p, bootStarted) {
			return fmt.Errorf("%s was stopped while it was being prepared", ws.Name)
		}
		m.phase(PhaseBoot, "")
		m.progressf("starting workspace %s", ws.Name)
		starts++
		r, err := m.spawnRunner(ws, p)
		if err != nil {
			return err
		}
		runner = r
		runnerDone.Store(false)
		go func() {
			err := r.Wait()
			runnerDone.Store(true)
			exited <- err
		}()
		return nil
	}
	if !m.Running(ws) {
		// One process prepares a workspace at a time; others wait, showing
		// its progress, instead of building the same image alongside it.
		unlock, err := m.waitLock(ctx, p.prepare, p.progress)
		if err != nil {
			return nil, err
		}
		if !m.Running(ws) {
			err = start()
		}
		unlock()
		if err != nil {
			return nil, err
		}
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
			if !m.Running(ws) && starts >= 3 {
				// No runner holds the workspace: ours failed for real.
				return nil, fmt.Errorf("workspace VM exited (%v); see %s and %s", err, p.runnerLog, p.console)
			}
			// Another runner won the race, or one was stopping (an idle
			// stop) when ours started; keep waiting, or start again below.
		case <-deadline:
			stop()
			return nil, fmt.Errorf("workspace VM did not answer within %s; see %s", bootTimeout, p.console)
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		// The workspace was running when we looked but has stopped since
		// (an idle stop, skerry down): start it again.
		if runner == nil && !m.Running(ws) && starts < 3 {
			unlock, err := m.waitLock(ctx, p.prepare, p.progress)
			if err != nil {
				return nil, err
			}
			if !m.Running(ws) {
				err = start()
			}
			unlock()
			if err != nil {
				return nil, err
			}
			deadline = time.After(bootTimeout) // the new runner gets its own time
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
	// Setup running now will fail as the guest's services stop; this marks
	// that as an interruption, not the project's failure.
	_ = os.WriteFile(p.stopping, nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(p.stopping, now, now)
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

// ResetData replaces a stopped workspace's service data: with a clone of
// snapshot if one is named; otherwise, with useDefault, with what a new
// workspace gets (the repository's default snapshot, or nothing); or else
// with an empty disk.
func (m *Manager) ResetData(ws registry.Workspace, snapshot string, useDefault bool) error {
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
	if snapshot != "" {
		// Clone first, so a wrong name loses nothing.
		tmp := p.dataDisk + ".new"
		_ = os.Remove(tmp)
		if _, err := m.cloneSnapshot(ws, snapshot, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, p.dataDisk); err != nil {
			os.Remove(tmp)
			return err
		}
		return removeIfExists(p.setup)
	}
	if err := removeIfExists(p.dataDisk); err != nil {
		return err
	}
	if err := removeIfExists(p.setup); err != nil {
		return err
	}
	if !useDefault {
		// An empty disk, so the default snapshot isn't cloned at start.
		return ensureSparse(p.dataDisk, dataDiskSize)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	for _, a := range proj.AllApps {
		for _, port := range a.Ports {
			want = append(want, Forward{Name: a.Name, Guest: port, Web: !a.NotHTTP})
		}
	}
	if len(want) > maxForwards {
		want = want[:maxForwards]
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
		listen := d.listen
		if f.Web {
			listen = d.listenWeb
		}
		// The port it had last time, else any. Never one the checkout names
		// (an app's own 3000, say): the VM could then pick a port the
		// user's own servers use on this Mac, and take their traffic.
		var l net.Listener
		err := errors.New("no port")
		if port := previous[f.Guest]; port != 0 && port != f.Guest {
			l, err = listen(port, f.Guest)
		}
		if err != nil {
			l, err = listen(0, f.Guest)
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

// ensureDisk makes the workspace's data disk if it has none: a clone of the
// repository's default snapshot if there is one, else a blank sparse
// file. It reports whether it made one.
func (m *Manager) ensureDisk(ws registry.Workspace, p wsPaths, size int64) (bool, error) {
	if info, err := os.Lstat(p.dataDisk); err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("%s is not a regular file", p.dataDisk)
		}
		return false, nil
	}
	if ok, err := m.cloneSnapshot(ws, DefaultSnapshot, p.dataDisk); ok || err != nil {
		if ok {
			fmt.Fprintf(os.Stderr, "data disk: a clone of snapshot %s\n", DefaultSnapshot)
		}
		return ok, err
	}
	return true, ensureSparse(p.dataDisk, size)
}

// ensureSparse creates a sparse disk image of size bytes if none exists.
func ensureSparse(path string, size int64) error {
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
