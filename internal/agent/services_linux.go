//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/services"
)

// Where services keep runtime files and logs in the guest. Both are in
// memory; only dataMount persists.
const (
	serviceRun  = "/run/skerry-services"
	serviceLogs = "/var/log/skerry"
	serviceLog  = 4 << 20
)

// Restart backoff for services that exit.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// A service up this long counts as healthy, so its backoff resets.
	healthyAfter = time.Minute
	// stopGrace is how long a service gets to stop cleanly. PID 1 waits
	// longer for the supervisor, and the host longer still for the VM.
	stopGrace = 10 * time.Second
)

// service is how the agent runs one preset.
type service struct {
	services.Preset
	uid, gid int
	secret   string // this service's own password
	dir      string // its directory on the data disk
	log      *fsutil.CappedLog

	mu       sync.Mutex
	pid      int
	running  bool // pid is a live process (cmd.Wait hasn't returned)
	stopping bool // the supervisor is shutting down
}

// RunServices runs and supervises the workspace's services until SIGTERM,
// which stops them cleanly. If it can't start them at all, it says why in
// a file WaitServices reports.
func RunServices() error {
	// Before anything else, so a stop that arrives during setup is a clean
	// one rather than Go's default exit.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	for _, d := range []string{serviceRun, serviceLogs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// A previous supervisor's state says nothing about this one's services.
	for _, pattern := range []string{"*.ready", "*.failed", "error"} {
		old, _ := filepath.Glob(filepath.Join(serviceRun, pattern))
		for _, f := range old {
			_ = os.Remove(f)
		}
	}
	errFile := filepath.Join(serviceRun, "error")
	err := runServices(stop)
	if err != nil {
		_ = os.WriteFile(errFile, []byte(err.Error()+"\n"), 0o644)
	}
	return err
}

func runServices(stop <-chan os.Signal) error {
	cfg, err := ReadConfig(shareMount)
	if err != nil {
		return err
	}
	if !isMountPoint(dataMount) {
		return fmt.Errorf("the data disk is not mounted at %s, so services can't keep their data; see skerry logs console", dataMount)
	}
	var all []*service
	for _, name := range cfg.Services {
		p, ok := services.Lookup(name)
		if !ok {
			return fmt.Errorf("unknown service %q", name)
		}
		uid, gid, err := ensureUser(p.User, p.UID)
		if err != nil {
			return err
		}
		log, err := fsutil.OpenCappedLog(filepath.Join(serviceLogs, p.Name+".log"), serviceLog)
		if err != nil {
			return err
		}
		all = append(all, &service{Preset: p, uid: uid, gid: gid, secret: services.Password(cfg.Secret, p.Name),
			dir: filepath.Join(dataMount, p.Name), log: log})
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func() { defer wg.Done(); s.supervise(ctx) }()
	}
	<-stop
	cancel()
	for _, s := range all {
		s.stop()
	}
	wg.Wait()
	return nil
}

// supervise sets the service up and keeps it running until ctx ends.
func (s *service) supervise(ctx context.Context) {
	backoff := minBackoff
	failures := 0
	failed := filepath.Join(serviceRun, s.Name+".failed")
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.logf("exited: %v", err)
		if time.Since(started) > healthyAfter {
			backoff, failures = minBackoff, 0
		}
		// After a few quick failures, tell whoever is waiting; keep
		// retrying in case it was something passing.
		if failures++; failures == 3 {
			_ = os.WriteFile(failed, []byte(fmt.Sprintf("exited: %v\n", err)), 0o644)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func (s *service) runOnce(ctx context.Context) error {
	ready := filepath.Join(serviceRun, s.Name+".ready")
	_ = os.Remove(ready)
	if err := s.setup(); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	cmd := s.command()
	cmd.Stdout, cmd.Stderr = s.log, s.log
	// A child that keeps the log pipe open (a forked save) mustn't hide
	// the service's own exit.
	cmd.WaitDelay = 3 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid)},
		Setpgid:    true,
		Pdeathsig:  syscall.SIGKILL,
	}
	// Starting and recording the process happen under the lock, after a
	// last look at whether a stop arrived during setup (initdb can take
	// seconds): either stop sees the process, or it is never started.
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.pid, s.running = cmd.Process.Pid, true
	s.mu.Unlock()
	s.logf("started (pid %d)", cmd.Process.Pid)

	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		exited <- err
	}()
	readyCtx, cancelReady := context.WithCancel(ctx)
	readyDone := make(chan struct{})
	go func() {
		defer close(readyDone)
		if err := s.whenUp(readyCtx); err != nil {
			if readyCtx.Err() == nil {
				s.logf("not ready: %v", err)
				// Tell whoever is waiting now, rather than at their timeout.
				_ = os.WriteFile(filepath.Join(serviceRun, s.Name+".failed"), []byte(err.Error()+"\n"), 0o644)
			}
			return
		}
		_ = os.WriteFile(ready, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
		_ = os.Remove(filepath.Join(serviceRun, s.Name+".failed"))
		s.logf("ready")
	}()
	// Whatever happens next, the readiness check is over before the
	// service is reported as gone, so it can't mark a dead service ready.
	finish := func() {
		cancelReady()
		<-readyDone
		_ = os.Remove(ready)
	}

	select {
	case err := <-exited:
		finish()
		return err
	case <-ctx.Done():
	}
	// Stopping: give the service time to shut down cleanly.
	select {
	case <-exited:
	case <-time.After(stopGrace):
		s.kill()
		<-exited
	}
	finish()
	return nil
}

// stop asks the service to shut down, and keeps it from starting again.
// Postgres gets a "fast" shutdown (SIGINT) sent to the postmaster alone,
// which then stops its backends in order; the others get SIGTERM.
func (s *service) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	if !s.running {
		return
	}
	if s.Name == "postgres" {
		_ = syscall.Kill(s.pid, syscall.SIGINT)
		return
	}
	_ = syscall.Kill(-s.pid, syscall.SIGTERM)
}

// kill ends the service's whole process group.
func (s *service) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		_ = syscall.Kill(-s.pid, syscall.SIGKILL)
	}
}

func (s *service) logf(format string, args ...any) {
	fmt.Fprintf(s.log, "skerry: %s: %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// setup prepares the service's data directory, creating it on first boot.
func (s *service) setup() error {
	if err := s.ownedDir(s.dir, 0o700); err != nil {
		return err
	}
	switch s.Name {
	case "postgres":
		if err := s.ownedDir("/run/postgresql", 0o755); err != nil {
			return err
		}
		data := filepath.Join(s.dir, "data")
		if _, err := os.Stat(data); err == nil {
			return nil
		}
		// The cluster is made under another name and renamed into place
		// only when complete, so a stop in the middle (a first boot cut
		// short) leaves nothing half-made behind.
		tmp := data + ".new"
		_ = os.RemoveAll(tmp)
		pwfile := filepath.Join(serviceRun, "postgres.pw")
		if err := s.ownedFile(pwfile, []byte(s.secret+"\n")); err != nil {
			return err
		}
		defer os.Remove(pwfile)
		if err := s.runAs("initdb", "-D", tmp, "-U", "postgres", "--pwfile="+pwfile,
			"--auth-local=trust", "--auth-host=scram-sha-256", "-E", "UTF8", "--no-locale"); err != nil {
			return err
		}
		// Listen on the guest's loopback only: the host reaches it through
		// the agent. TCP connections need the password.
		if err := appendFile(filepath.Join(tmp, "postgresql.conf"),
			"\n# skerry\nlisten_addresses = 'localhost'\nunix_socket_directories = '/run/postgresql'\n"); err != nil {
			return err
		}
		return os.Rename(tmp, data)
	case "valkey":
		conf := strings.Join([]string{
			"bind 127.0.0.1 -::1",
			"port 6379",
			"protected-mode yes",
			"daemonize no",
			"dir " + s.dir,
			"appendonly yes",
			"requirepass " + s.secret,
		}, "\n") + "\n"
		return s.ownedFile(filepath.Join(serviceRun, "valkey.conf"), []byte(conf))
	}
	return nil
}

func (s *service) command() *exec.Cmd {
	var cmd *exec.Cmd
	switch s.Name {
	case "postgres":
		cmd = command("postgres", "-D", filepath.Join(s.dir, "data"))
	case "valkey":
		cmd = command("valkey-server", filepath.Join(serviceRun, "valkey.conf"))
	case "temporal":
		cmd = command("temporal", "server", "start-dev",
			"--ip", "127.0.0.1", "--port", "7233", "--ui-port", "8233",
			"--db-filename", filepath.Join(s.dir, "temporal.db"),
			"--namespace", "default", "--log-level", "warn")
	}
	cmd.Dir = s.dir
	cmd.Env = append(cmd.Env, "HOME="+s.dir)
	return cmd
}

// whenUp waits until the service answers, then finishes first-boot setup.
func (s *service) whenUp(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	if err := waitPort(ctx, s.Ports[0].Guest, time.Until(deadline)); err != nil {
		return err
	}
	// The port can open before the service accepts work (Postgres
	// replaying its log, Temporal creating its namespace), so retry.
	for {
		err := s.finishSetup()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// finishSetup runs the steps that need the service up; each is safe to
// repeat.
func (s *service) finishSetup() error {
	switch s.Name {
	case "postgres":
		// The password is set on every boot, so it always matches the
		// workspace's secret. The secret is hex, so it needs no quoting.
		if err := s.psql("postgres", "ALTER USER postgres PASSWORD '"+s.secret+"'"); err != nil {
			return err
		}
		out, err := s.output("psql", "-h", "/run/postgresql", "-U", "postgres", "-d", "postgres", "-tAc",
			"SELECT 1 FROM pg_database WHERE datname = '"+services.Database+"'")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "1" {
			if err := s.psql("postgres", "CREATE DATABASE "+services.Database); err != nil {
				return err
			}
		}
		return s.psql(services.Database, "CREATE EXTENSION IF NOT EXISTS vector")
	case "temporal":
		if _, err := s.output("temporal", "operator", "namespace", "describe", "--namespace", "default", "--address", "127.0.0.1:7233"); err != nil {
			return errors.New("the default namespace isn't there yet")
		}
	}
	return nil
}

func (s *service) psql(db, sql string) error {
	_, err := s.output("psql", "-h", "/run/postgresql", "-U", "postgres", "-d", db, "-v", "ON_ERROR_STOP=1", "-qc", sql)
	return err
}

// output runs a tool as the service's user.
func (s *service) output(name string, args ...string) (string, error) {
	cmd := command(name, args...)
	cmd.Dir = s.dir
	cmd.Env = append(cmd.Env, "HOME="+s.dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid)},
		Pdeathsig:  syscall.SIGKILL, // never outlive a supervisor that died
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %v: %s", name, err, bytes.TrimSpace(errb.Bytes()))
	}
	return out.String(), nil
}

func (s *service) runAs(name string, args ...string) error {
	out, err := s.output(name, args...)
	if out != "" {
		fmt.Fprint(s.log, out)
	}
	return err
}

func (s *service) ownedDir(dir string, perm os.FileMode) error {
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	if err := os.Chmod(dir, perm); err != nil {
		return err
	}
	return os.Lchown(dir, s.uid, s.gid)
}

func (s *service) ownedFile(path string, data []byte) error {
	_ = os.Remove(path)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Lchown(path, s.uid, s.gid)
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func waitPort(ctx context.Context, port uint16, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d never opened: %w", port, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ensureUser makes sure name exists in /etc/passwd and /etc/group, adding
// it with uid if not, and returns its ids. /etc is in the in-memory
// overlay, so this happens every boot and never touches the image.
func ensureUser(name string, uid int) (int, int, error) {
	if u, g, ok := lookupUser(name); ok {
		if u == 0 {
			return 0, 0, fmt.Errorf("user %s is root; refusing to run a service as root", name)
		}
		return u, g, nil
	}
	home := filepath.Join(dataMount, name)
	if err := appendFile("/etc/group", fmt.Sprintf("%s:x:%d:\n", name, uid)); err != nil {
		return 0, 0, err
	}
	if err := appendFile("/etc/passwd", fmt.Sprintf("%s:x:%d:%d:%s:%s:/sbin/nologin\n", name, uid, uid, name, home)); err != nil {
		return 0, 0, err
	}
	return uid, uid, nil
}

func lookupUser(name string) (uid, gid int, ok bool) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		u, err1 := strconv.Atoi(fields[2])
		g, err2 := strconv.Atoi(fields[3])
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return u, g, true
	}
	return 0, 0, false
}

// WaitServices waits until every configured service is ready, printing each
// one's state. It is what `skerry up` runs in the guest.
func WaitServices(timeout time.Duration) error {
	cfg, err := ReadConfig(shareMount)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for _, name := range cfg.Services {
		for {
			if _, err := os.Stat(filepath.Join(serviceRun, name+".ready")); err == nil {
				break
			}
			if msg, err := os.ReadFile(filepath.Join(serviceRun, "error")); err == nil {
				return errors.New(strings.TrimSpace(string(msg)))
			}
			if msg, err := os.ReadFile(filepath.Join(serviceRun, name+".failed")); err == nil {
				return fmt.Errorf("%s keeps failing (%s); see skerry logs %s", name, strings.TrimSpace(string(msg)), name)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s is not ready; see skerry logs %s", name, name)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}
