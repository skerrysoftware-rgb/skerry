package agent

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// connTo serves srv on a local socket and returns a connection to it.
// (net.Pipe is unbuffered, which deadlocks SSH's simultaneous version
// exchange; real sockets, vsock included, are buffered.)
func connTo(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.Serve(l)
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// pair starts a server and a connected client over an in-memory pipe.
func pair(t *testing.T) (*Client, *Server) {
	t.Helper()
	state, share := t.TempDir(), t.TempDir()
	keys, err := EnsureKeys(state, share)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := loadSigner(filepath.Join(share, HostKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(hostKey, keys.Client.PublicKey())
	c, err := NewClient(connTo(t, srv), keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv
}

func TestRun(t *testing.T) {
	c, _ := pair(t)
	ctx := context.Background()
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	code, err := c.Run(ctx, Cmd{
		Args:   []string{"sh", "-c", `echo "out:$GREETING:$(pwd)"; echo "err" >&2; read line; echo "in:$line"; exit 7`},
		Dir:    dir,
		Env:    map[string]string{"GREETING": "it's here"},
		Stdin:  strings.NewReader("typed\n"),
		Stdout: &out, Stderr: &errOut,
	})
	if err != nil {
		t.Fatal(err)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if code != 7 {
		t.Errorf("exit = %d, want 7", code)
	}
	if want := "out:it's here:" + realDir + "\nin:typed\n"; out.String() != want && out.String() != strings.Replace(want, realDir, dir, 1) {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "err\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestArgsAreNotInterpreted(t *testing.T) {
	c, _ := pair(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	var out, errOut bytes.Buffer
	code, err := c.Run(context.Background(), Cmd{
		Args:   []string{"echo", "$(touch " + marker + ")", "a'b", "`id`", "; rm -rf /"},
		Stdout: &out, Stderr: &errOut,
	})
	if err != nil || code != 0 {
		t.Fatalf("run = %d, %v (%s)", code, err, errOut.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an argument was executed by the shell")
	}
	if want := "$(touch " + marker + ") a'b `id` ; rm -rf /\n"; out.String() != want {
		t.Errorf("echo printed %q, want %q", out.String(), want)
	}
}

func TestBaseEnvCannotBeOverridden(t *testing.T) {
	c, _ := pair(t)
	var out bytes.Buffer
	_, err := c.Run(context.Background(), Cmd{
		Args: []string{"sh", "-c", "echo $PATH"}, Env: map[string]string{"PATH": "/evil"}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "/evil") {
		t.Errorf("client overrode PATH: %s", out.String())
	}
}

func TestRejectsBadEnvNames(t *testing.T) {
	c, _ := pair(t)
	for _, name := range []string{"BAD NAME", "1X", "A=B", ""} {
		_, err := c.Run(context.Background(), Cmd{Args: []string{"true"}, Env: map[string]string{name: "x"}})
		if err == nil {
			t.Errorf("env name %q accepted", name)
		}
	}
}

func TestContextCancelStopsCommand(t *testing.T) {
	c, _ := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Run(ctx, Cmd{Args: []string{"sleep", "30"}})
	if err == nil {
		t.Fatal("cancelled run returned no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the command promptly")
	}
}

func TestDisconnectKillsCommand(t *testing.T) {
	c, _ := pair(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	go c.Run(context.Background(), Cmd{Args: []string{"sh", "-c", "echo $$ > " + pidFile + "; exec sleep 30"}})
	var pid int
	for i := 0; i < 50 && pid == 0; i++ {
		time.Sleep(50 * time.Millisecond)
		if data, err := os.ReadFile(pidFile); err == nil {
			_, _ = fmtSscan(strings.TrimSpace(string(data)), &pid)
		}
	}
	if pid == 0 {
		t.Fatal("command never started")
	}
	c.Close()
	for i := 0; i < 100; i++ {
		if !alive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("process %d still running after the client disconnected", pid)
}

func TestWrongClientKeyRejected(t *testing.T) {
	state, share := t.TempDir(), t.TempDir()
	keys, err := EnsureKeys(state, share)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _ := loadSigner(filepath.Join(share, HostKeyFile))
	other, err := EnsureKeys(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(hostKey, keys.Client.PublicKey())
	if _, err := NewClient(connTo(t, srv), Keys{Client: other.Client, HostPublic: keys.HostPublic}); err == nil {
		t.Fatal("server accepted an unknown client key")
	}
}

func TestWrongHostKeyRejected(t *testing.T) {
	keys, err := EnsureKeys(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	impostorShare := t.TempDir()
	if _, err := EnsureKeys(t.TempDir(), impostorShare); err != nil {
		t.Fatal(err)
	}
	fakeHost, err := loadSigner(filepath.Join(impostorShare, HostKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(fakeHost, keys.Client.PublicKey())
	if _, err := NewClient(connTo(t, srv), keys); err == nil {
		t.Fatal("client accepted a host key other than the pinned one")
	}
}

func TestPingAndPoweroff(t *testing.T) {
	c, srv := pair(t)
	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := c.Poweroff(); err == nil {
		t.Fatal("poweroff accepted with no handler")
	}
	called := make(chan struct{})
	srv.Poweroff = func() { close(called) }
	if err := c.Poweroff(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("poweroff handler not called")
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		cfg Config
		ok  bool
	}{
		{Config{Mode: ModeServe, WorkspacePath: "/Users/me/src/app/.claude/worktrees/x"}, true},
		{Config{Mode: ModeFinalize}, true},
		{Config{Mode: ModeServe, WorkspacePath: "relative"}, false},
		{Config{Mode: ModeServe, WorkspacePath: "/"}, false},
		{Config{Mode: ModeServe, WorkspacePath: "/etc"}, false},
		{Config{Mode: ModeServe, WorkspacePath: "/usr/local/x"}, false},
		{Config{Mode: ModeServe, WorkspacePath: "/home/x/../../etc"}, false},
		{Config{Mode: ModeServe, WorkspacePath: "/home/x", Hostname: "Bad_Name"}, false},
		{Config{Mode: "other"}, false},
	} {
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: err=%v want ok=%v", tc.cfg, err, tc.ok)
		}
	}
}
