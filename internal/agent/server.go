package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Global requests the agent understands.
const (
	RequestPing     = "ping@skerry"
	RequestPoweroff = "poweroff@skerry"
)

// EnvCwd carries the working directory for a command.
const EnvCwd = "SKERRY_CWD"

// Limits on what a client may send.
const (
	maxEnvVars  = 128
	maxEnvValue = 32 << 10
	maxCommand  = 256 << 10
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// baseEnv is every command's starting environment. The host's PATH and HOME
// mean nothing in the guest, so clients cannot override these.
var baseEnv = map[string]string{
	"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME": "/root",
}

// Server runs commands for authenticated SSH clients.
type Server struct {
	config *ssh.ServerConfig
	// Shell runs command strings; tests point it at the host's shell.
	Shell string
	// Poweroff is called for a poweroff request; nil refuses the request.
	Poweroff func()
	// Env is added to every command's environment; the client's variables
	// win over it, and baseEnv wins over both.
	Env map[string]string
	Log *log.Logger
}

// NewServer builds a server that accepts only authorizedKey.
func NewServer(hostKey ssh.Signer, authorizedKey ssh.PublicKey) *Server {
	want := authorizedKey.Marshal()
	cfg := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), want) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostKey)
	return &Server{config: cfg, Shell: "/bin/sh", Log: log.New(io.Discard, "", 0)}
}

// Serve accepts connections until l is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.ServeConn(conn)
	}
}

// ServeConn handles one connection.
func (s *Server) ServeConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second)) // handshake only
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.Log.Printf("handshake: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer sconn.Close()
	go s.globalRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go s.session(ch, chReqs)
		case "direct-tcpip":
			go s.directTCPIP(nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "only session and direct-tcpip channels")
		}
	}
}

// directTCPIP connects the host to a port on the guest's loopback
// interface: that is how skerry forwards ports, so servers that listen
// only on localhost in the VM are reachable too. Nothing else can be
// dialed through it.
func (s *Server) directTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "bad request")
		return
	}
	host := ""
	switch p.Host {
	case "127.0.0.1", "localhost":
		host = "127.0.0.1"
	case "::1":
		host = "::1"
	}
	if host == "" || p.Port == 0 || p.Port > 65535 {
		_ = nc.Reject(ssh.Prohibited, "only ports on the guest's loopback interface")
		return
	}
	out, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(p.Port))), 5*time.Second)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		out.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(out, ch)
		if tc, ok := out.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ch, out)
		_ = ch.CloseWrite()
		done <- struct{}{}
	}()
	<-done
	<-done
	ch.Close()
	out.Close()
}

func (s *Server) globalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case RequestPing:
			_ = req.Reply(true, nil)
		case RequestPoweroff:
			if s.Poweroff == nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			go s.Poweroff()
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// session handles env and exec requests, then runs one command.
func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	env := map[string]string{}
	var proc *exec.Cmd
	done := make(chan struct{})
	var mu sync.Mutex // guards proc

	for req := range reqs {
		switch req.Type {
		case "env":
			var kv struct{ Name, Value string }
			ok := ssh.Unmarshal(req.Payload, &kv) == nil &&
				envName.MatchString(kv.Name) && len(kv.Value) <= maxEnvValue &&
				len(env) < maxEnvVars && !strings.ContainsRune(kv.Value, 0)
			if ok {
				env[kv.Name] = kv.Value
			}
			_ = req.Reply(ok, nil)
		case "exec":
			var p struct{ Command string }
			mu.Lock()
			started := proc != nil
			mu.Unlock()
			if started || ssh.Unmarshal(req.Payload, &p) != nil || len(p.Command) > maxCommand || strings.ContainsRune(p.Command, 0) {
				_ = req.Reply(false, nil)
				continue
			}
			cmd, err := s.start(p.Command, env, ch)
			if err != nil {
				_ = req.Reply(false, nil)
				fmt.Fprintf(ch.Stderr(), "skerry agent: %v\n", err)
				return
			}
			_ = req.Reply(true, nil)
			mu.Lock()
			proc = cmd
			mu.Unlock()
			go func() {
				err := cmd.Wait()
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus(cmd, err)}))
				_ = ch.CloseWrite()
				close(done)
				_ = ch.Close()
			}()
		case "signal":
			var p struct{ Signal string }
			ok := ssh.Unmarshal(req.Payload, &p) == nil
			sig, known := signals[p.Signal]
			ok = ok && known
			mu.Lock()
			cmd := proc
			mu.Unlock()
			if ok && cmd != nil {
				_ = syscall.Kill(-cmd.Process.Pid, sig)
			}
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}
		default:
			// pty-req, shell, subsystem, x11, agent forwarding: not offered.
			_ = req.Reply(false, nil)
		}
	}

	// The client went away. Stop the command if it is still running.
	mu.Lock()
	cmd := proc
	mu.Unlock()
	if cmd == nil {
		return
	}
	select {
	case <-done:
	default:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
}

var signals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

func (s *Server) start(command string, clientEnv map[string]string, ch ssh.Channel) (*exec.Cmd, error) {
	cmd := exec.CommandContext(context.Background(), s.Shell, "-c", command)
	cmd.Dir = "/"
	if cwd := clientEnv[EnvCwd]; cwd != "" {
		if !filepath.IsAbs(cwd) {
			return nil, fmt.Errorf("working directory %q is not absolute", cwd)
		}
		cmd.Dir = filepath.Clean(cwd)
	}
	env := map[string]string{}
	for k, v := range s.Env {
		env[k] = v
	}
	for k, v := range clientEnv {
		env[k] = v
	}
	delete(env, EnvCwd)
	for k, v := range baseEnv {
		env[k] = v
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Stdin is copied by a goroutine Wait doesn't track: a client whose
	// stdin never ends (a terminal) must not hold up the exit.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	// Its own process group, so signals and cleanup reach its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A background child holding stdout open delays the result this long.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_, _ = io.Copy(stdin, ch)
		_ = stdin.Close()
	}()
	return cmd, nil
}

// exitStatus is the command's own status. It reads the process state, not
// Wait's error, so an I/O timeout from a lingering background child doesn't
// replace the real exit code.
func exitStatus(cmd *exec.Cmd, err error) uint32 {
	ps := cmd.ProcessState
	if ps == nil {
		return 255
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + uint32(ws.Signal())
	}
	if code := ps.ExitCode(); code >= 0 {
		return uint32(code)
	}
	if err != nil {
		return 255
	}
	return 0
}
