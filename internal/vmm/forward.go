package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	// act counts forwarded connections as use, for idle stop.
	act *activity
	// dialFunc replaces dialing the guest, in tests.
	dialFunc func(port uint16) (net.Conn, error)
	// own is the workspace's name on this machine (NAME.localhost), which
	// apps see as localhost.
	own string
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
	if d.dialFunc != nil {
		return d.dialFunc(port)
	}
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
				defer d.act.begin()()
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

// halfCloseGrace is how long the other direction may go on after one
// direction of a forwarded connection ends. A peer that never finishes (a
// guest holding a connection open) would otherwise keep it, and the
// workspace, busy forever.
const halfCloseGrace = 30 * time.Second

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
	select {
	case <-done:
	case <-time.After(halfCloseGrace):
	}
	a.Close()
	b.Close()
}

// listenWeb forwards host loopback port hostPort to an HTTP server on
// guestPort, as an HTTP proxy that makes requests look as they do inside
// the VM: Host is localhost:guestPort (apps often check it), a same-origin
// Origin says so too, and redirects back to that address are pointed at
// the address the browser used. WebSockets and streamed responses pass
// through.
func (d *guestDialer) listenWeb(hostPort, guestPort uint16) (net.Listener, error) {
	l, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), hostPort).String())
	if err != nil {
		return nil, err
	}
	guest := strconv.Itoa(int(guestPort))
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	// The app sees the .localhost name the browser used, at its own port,
	// so apps that serve several names (docs.app.localhost) still can;
	// the workspace's own name and loopback addresses read as localhost.
	inner := func(outer string) string {
		h, _, _ := net.SplitHostPort(outer)
		if h = strings.ToLower(strings.TrimSuffix(h, ".")); h == d.own || !strings.HasSuffix(h, ".localhost") {
			h = "localhost"
		}
		return net.JoinHostPort(h, guest)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			in := inner(pr.In.Host)
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", in, in
			if o := pr.In.Header.Get("Origin"); o == "http://"+pr.In.Host {
				pr.Out.Header.Set("Origin", "http://"+in)
			}
		},
		Transport: &http.Transport{
			DialContext:           func(context.Context, string, string) (net.Conn, error) { return d.dial(guestPort) },
			MaxIdleConns:          16,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute,
		},
		ModifyResponse: func(r *http.Response) error {
			// A redirect to the app's own address (any local name, at its
			// port) goes back through the forward.
			// (http://localhost:3000/x, or //localhost:3000/x)
			if loc, err := url.Parse(r.Header.Get("Location")); err == nil && (loc.Scheme == "http" || loc.Scheme == "" && loc.Host != "") &&
				localHost(loc.Host, guest) {
				h := loc.Hostname()
				if h == "localhost" && r.Request != nil {
					if outer, _ := r.Request.Context().Value(outerHost{}).(string); outer != "" {
						h, _, _ = net.SplitHostPort(outer)
					}
				}
				loc.Host = net.JoinHostPort(h, port)
				r.Header.Set("Location", loc.String())
			}
			return nil
		},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only for this machine's names: the app's own checks of Host
			// and Origin (against DNS rebinding) see localhost, so the proxy
			// must not let a page on another name through as localhost.
			if !localHost(r.Host, port) {
				http.Error(w, "skerry: this port answers only to localhost names", http.StatusMisdirectedRequest)
				return
			}
			defer d.act.begin()()
			proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), outerHost{}, r.Host)))
		}),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = srv.Serve(l) }()
	return l, nil
}

// outerHost carries the address the browser used, from the handler to
// ModifyResponse.
type outerHost struct{}

// localHost reports whether a request's Host names this machine, at the
// port it came to: localhost, 127.0.0.1, [::1], or a .localhost name.
func localHost(host, port string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != port {
		return false
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}
