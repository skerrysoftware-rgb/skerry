package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client runs commands through a workspace's agent.
type Client struct {
	conn *ssh.Client
}

// NewClient performs the SSH handshake over conn, accepting only the
// workspace's pinned host key.
func NewClient(conn net.Conn, keys Keys) (*Client, error) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, "agent", &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(keys.Client)},
		HostKeyCallback: ssh.FixedHostKey(keys.HostPublic),
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("agent handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{conn: ssh.NewClient(c, chans, reqs)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Ping checks that the agent is answering.
func (c *Client) Ping() error {
	ok, _, err := c.conn.SendRequest(RequestPing, true, nil)
	if err == nil && !ok {
		err = errors.New("agent refused ping")
	}
	return err
}

// Poweroff asks the guest to shut down cleanly.
func (c *Client) Poweroff() error {
	ok, _, err := c.conn.SendRequest(RequestPoweroff, true, nil)
	if err == nil && !ok {
		err = errors.New("agent refused poweroff")
	}
	return err
}

// Cmd is a command to run in the guest.
type Cmd struct {
	// Args are run as-is: each is quoted, so nothing is interpreted by a shell.
	Args []string
	// Dir is the working directory in the guest.
	Dir    string
	Env    map[string]string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Signals forwards these signals to the command until it exits.
	Signals <-chan os.Signal
}

// Run runs cmd and returns its exit status.
func (c *Client) Run(ctx context.Context, cmd Cmd) (int, error) {
	if len(cmd.Args) == 0 {
		return 0, errors.New("no command")
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	for k, v := range cmd.Env {
		if err := sess.Setenv(k, v); err != nil {
			return 0, fmt.Errorf("the agent refused environment variable %s", k)
		}
	}
	if cmd.Dir != "" {
		if err := sess.Setenv(EnvCwd, cmd.Dir); err != nil {
			return 0, err
		}
	}
	sess.Stdout, sess.Stderr = cmd.Stdout, cmd.Stderr
	// The session's own stdin copy is waited for by Wait, which would hang
	// on a terminal that never sends end-of-input. Copy it ourselves and
	// let the copy go when the command exits.
	var stdin io.WriteCloser
	if cmd.Stdin != nil {
		if stdin, err = sess.StdinPipe(); err != nil {
			return 0, err
		}
	}

	quoted := make([]string, len(cmd.Args))
	for i, a := range cmd.Args {
		quoted[i] = shellQuote(a)
	}
	if err := sess.Start("exec " + strings.Join(quoted, " ")); err != nil {
		return 0, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(stdin, cmd.Stdin)
			_ = stdin.Close()
		}()
	}

	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	for {
		select {
		case err := <-waited:
			var exitErr *ssh.ExitError
			switch {
			case err == nil:
				return 0, nil
			case errors.As(err, &exitErr):
				return exitErr.ExitStatus(), nil
			default:
				return 0, err
			}
		case sig := <-cmd.Signals:
			if name, ok := signalNames[sig]; ok {
				_ = sess.Signal(name)
			}
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGTERM)
			return 0, ctx.Err()
		}
	}
}

var signalNames = map[os.Signal]ssh.Signal{
	syscall.SIGINT: ssh.SIGINT, syscall.SIGTERM: ssh.SIGTERM, syscall.SIGHUP: ssh.SIGHUP,
	syscall.SIGQUIT: ssh.SIGQUIT, syscall.SIGUSR1: ssh.SIGUSR1, syscall.SIGUSR2: ssh.SIGUSR2,
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
