//go:build linux

package agent

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// Paths the agent uses inside the guest.
const (
	agentPath   = "/.skerry/agent"
	scriptsDir  = "/.skerry/scripts"
	shareMount  = "/run/skerry"
	initConfig  = "/mnt/config"
	finalizedAt = "/.skerry/finalized"
)

var logger = log.New(os.Stderr, "skerry-agent: ", log.Lmsgprefix)

// Stage1 runs as /init from the initramfs. It loads the drivers, assembles
// the real root filesystem, switches to it and execs the agent from there,
// which keeps PID 1.
func Stage1() error {
	for _, m := range []struct{ src, dst, typ string }{
		{"proc", "/proc", "proc"}, {"sysfs", "/sys", "sysfs"}, {"devtmpfs", "/dev", "devtmpfs"},
	} {
		if err := unix.Mount(m.src, m.dst, m.typ, unix.MS_NOSUID|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	if err := loadModules("/lib/modules"); err != nil {
		return err
	}

	if err := mkdirMount(TagConfig, initConfig, "virtiofs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return err
	}
	cfg, err := ReadConfig(initConfig)
	if err != nil {
		return err
	}

	if cfg.Mode == ModeFinalize {
		// Install scripts write into the image itself.
		if err := mkdirMount(TagRootfs, "/newroot", "virtiofs", 0, ""); err != nil {
			return err
		}
	} else {
		// The image is shared read-only; writes land in memory and vanish
		// at the next boot.
		if err := mkdirMount(TagRootfs, "/mnt/lower", "virtiofs", unix.MS_RDONLY, ""); err != nil {
			return err
		}
		if err := mkdirMount("tmpfs", "/mnt/rw", "tmpfs", 0, "mode=0755"); err != nil {
			return err
		}
		for _, d := range []string{"/mnt/rw/upper", "/mnt/rw/work"} {
			if err := os.Mkdir(d, 0o755); err != nil {
				return err
			}
		}
		if err := unix.Mount("overlay", "/newroot", "overlay", 0,
			"lowerdir=/mnt/lower,upperdir=/mnt/rw/upper,workdir=/mnt/rw/work"); err != nil {
			return fmt.Errorf("mount overlay: %w", err)
		}
	}

	if err := mkdirMount("tmpfs", "/newroot/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return err
	}
	if err := os.MkdirAll("/newroot"+shareMount, 0o700); err != nil {
		return err
	}
	if err := unix.Mount(initConfig, "/newroot"+shareMount, "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move config share: %w", err)
	}
	if cfg.Mode == ModeServe {
		// The checkout appears at the same path as on the host.
		if err := mkdirMount(TagWork, "/newroot"+cfg.WorkspacePath, "virtiofs", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return err
		}
	}
	for _, d := range []string{"/dev", "/proc", "/sys"} {
		if err := os.MkdirAll("/newroot"+d, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(d, "/newroot"+d, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s: %w", d, err)
		}
	}

	// Free the initramfs's memory before leaving it behind.
	_ = os.RemoveAll("/lib/modules")
	_ = os.Remove("/init")

	if err := os.Chdir("/newroot"); err != nil {
		return err
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("switch root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	return unix.Exec(agentPath, []string{agentPath, "pid1"}, os.Environ())
}

func mkdirMount(src, dst, typ string, flags uintptr, data string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(src, dst, typ, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s: %w", typ, dst, err)
	}
	return nil
}

func loadModules(dir string) error {
	order, err := os.ReadFile(filepath.Join(dir, "load-order"))
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(order)) {
		f, err := os.Open(filepath.Join(dir, name+".ko"))
		if err != nil {
			return err
		}
		err = unix.FinitModule(int(f.Fd()), "", 0)
		f.Close()
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("load module %s: %w", name, err)
		}
	}
	return nil
}

// PID1 runs in the real root. It sets up the remaining mounts, then either
// runs install scripts (finalize) or starts the server and reaps orphans.
func PID1() error {
	for _, m := range []struct{ src, dst, typ, data string }{
		{"tmpfs", "/tmp", "tmpfs", "mode=1777"},
		{"devpts", "/dev/pts", "devpts", "gid=5,mode=620,ptmxmode=666"},
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777"},
	} {
		if err := mkdirMount(m.src, m.dst, m.typ, unix.MS_NOSUID|unix.MS_NODEV, m.data); err != nil {
			logger.Print(err)
		}
	}
	cfg, err := ReadConfig(shareMount)
	if err != nil {
		return err
	}
	if err := loopbackUp(); err != nil {
		logger.Printf("loopback: %v", err)
	}
	if cfg.Hostname != "" {
		_ = unix.Sethostname([]byte(cfg.Hostname))
		addHostsEntry(cfg.Hostname)
	}
	if cfg.Mode == ModeFinalize {
		err := finalize()
		if err != nil {
			logger.Printf("finalize failed: %v", err)
		}
		poweroff()
		return err
	}

	// The server runs as a child; PID 1 only reaps. Orphaned processes are
	// reparented here, and waiting for them keeps zombies from piling up.
	signal.Ignore(syscall.SIGHUP)
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGPWR)
	child := startServe()
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		switch {
		case err == nil && pid == child:
			logger.Printf("server exited (%v); restarting", ws)
			time.Sleep(time.Second)
			child = startServe()
		case err == nil && pid > 0:
			continue // reaped an orphan; look for more
		}
		select {
		case <-sigterm:
			poweroff()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// loopbackUp brings up lo; the kernel then assigns 127.0.0.1 and ::1.
// Without it nothing in the guest can reach localhost.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// addHostsEntry makes the workspace's hostname resolve locally, as most
// tools expect. /etc lives in the in-memory overlay, so this never reaches
// the shared image.
func addHostsEntry(name string) {
	f, err := os.OpenFile("/etc/hosts", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		logger.Printf("hosts: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "127.0.1.1\t%s\n", name)
}

func startServe() int {
	cmd := exec.Command(agentPath, "serve")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		logger.Printf("start server: %v", err)
		return -1
	}
	return cmd.Process.Pid
}

// Serve runs the SSH server on vsock.
func Serve() error {
	hostKey, err := loadSigner(filepath.Join(shareMount, HostKeyFile))
	if err != nil {
		return err
	}
	authorized, err := os.ReadFile(filepath.Join(shareMount, AuthorizedKeyFile))
	if err != nil {
		return err
	}
	clientKey, _, _, _, err := ssh.ParseAuthorizedKey(authorized)
	if err != nil {
		return fmt.Errorf("authorized key: %w", err)
	}
	srv := NewServer(hostKey, clientKey)
	srv.Log = logger
	srv.Poweroff = poweroff
	l, err := listenVsock(Port)
	if err != nil {
		return err
	}
	logger.Printf("ready on vsock port %d", Port)
	return srv.Serve(l)
}

// finalize runs each package's install scripts once, in install order.
func finalize() error {
	if _, err := os.Stat(finalizedAt); err == nil {
		return nil
	}
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	env := []string{"PATH=" + baseEnv["PATH"], "HOME=/root"}
	for _, name := range names {
		logger.Printf("running %s", name)
		cmd := exec.Command("/bin/sh", filepath.Join(scriptsDir, name))
		cmd.Dir, cmd.Env = "/", env
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			// Alpine's own installer reports script failures and carries on.
			logger.Printf("%s: %v", name, err)
		}
	}
	return os.WriteFile(finalizedAt, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

func poweroff() {
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		logger.Printf("poweroff: %v", err)
	}
}

// listenVsock listens on a vsock port. The net package has no vsock
// support, so accept and wrap file descriptors directly.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 32); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &vsockListener{fd: fd, port: port}, nil
}

type vsockListener struct {
	fd   int
	port uint32
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		nfd, _, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := unix.SetNonblock(nfd, true); err != nil {
			unix.Close(nfd)
			return nil, err
		}
		// A non-blocking descriptor joins Go's poller, so reads, writes and
		// deadlines behave like any other connection.
		return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock"), port: l.port}, nil
	}
}

func (l *vsockListener) Close() error   { return unix.Close(l.fd) }
func (l *vsockListener) Addr() net.Addr { return vsockAddr(l.port) }

type vsockConn struct {
	*os.File
	port uint32
}

func (c *vsockConn) LocalAddr() net.Addr  { return vsockAddr(c.port) }
func (c *vsockConn) RemoteAddr() net.Addr { return vsockAddr(0) }

type vsockAddr uint32

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock:%d", uint32(a)) }
