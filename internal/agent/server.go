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
	Log      *log.Logger
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
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.session(ch, chReqs)
	}
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
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus(err)}))
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
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	// Its own process group, so signals and cleanup reach its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func exitStatus(err error) uint32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + uint32(ws.Signal())
		}
		return uint32(exitErr.ExitCode())
	}
	return 255
}
