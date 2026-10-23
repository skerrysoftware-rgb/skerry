package vmm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
	"github.com/skerrysoftware-rgb/skerry/internal/engine"
)

// guestDialer reaches ports on the guest's loopback interface through the
// agent's SSH connection, which it keeps open and reopens when it breaks.
type guestDialer struct {
	vm   *engine.VM
	keys agent.Keys
	mu   sync.Mutex
	c    *agent.Client
	// open holds forwarded connections, so stopping ends them too.
	open map[net.Conn]bool
}

// track records conn until it is closed; false means the dialer is
// already closed.
func (d *guestDialer) track(conn net.Conn, add bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !add {
		delete(d.open, conn)
		return true
	}
	if d.open == nil {
		return false
	}
	d.open[conn] = true
	return true
}

func (d *guestDialer) client() (*agent.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.c != nil {
		return d.c, nil
	}
	conn, err := d.vm.Dial(agent.Port)
	if err != nil {
		return nil, err
	}
	c, err := agent.NewClient(conn, d.keys)
	if err != nil {
		return nil, err
	}
	d.c = c
	return c, nil
}

func (d *guestDialer) drop(c *agent.Client) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.c == c {
		d.c.Close()
		d.c = nil
	}
}

func (d *guestDialer) dial(port uint16) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		c, err := d.client()
		if err != nil {
			return nil, err
		}
		conn, err := c.DialGuest(port)
		var refused *ssh.OpenChannelError
		if err == nil || errors.As(err, &refused) || attempt == 1 {
			return conn, err // a refusal is the guest's answer
		}
		d.drop(c) // the connection broke (the agent restarted); reconnect
	}
}

func (d *guestDialer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.c != nil {
		d.c.Close()
		d.c = nil
	}
	for conn := range d.open {
		conn.Close()
	}
	d.open = nil
}

// listen forwards host loopback port hostPort (0 picks one) to guestPort
// on the guest's loopback interface.
func (d *guestDialer) listen(hostPort, guestPort uint16) (net.Listener, error) {
	l, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), hostPort).String())
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				// Out of descriptors or similar: wait rather than stop
				// forwarding for good.
				fmt.Fprintf(os.Stderr, "forward %d: %v\n", guestPort, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			go func() {
				if !d.track(c, true) {
					c.Close()
					return
				}
				defer d.track(c, false)
				g, err := d.dial(guestPort)
				if err != nil {
					c.Close()
					return
				}
				pipe(c, g)
			}()
		}
	}()
	return l, nil
}

// pipe copies both ways, passing half-closes through, then closes both.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			_ = cw.CloseWrite()
		} else {
			// An error, or no half-close: end the whole connection.
			a.Close()
			b.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
