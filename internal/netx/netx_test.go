package netx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// guest builds a stand-in for the VM: its own stack at GuestIP, wired to the
// network through a real frame socket pair, as the VM's network card is.
func guest(t *testing.T, n *Network) *stack.Stack {
	t.Helper()
	return guestVia(t, n, func(f *os.File) io.ReadWriteCloser { return f })
}

// guestVia is guest with the host end wrapped, to inject faults.
func guestVia(t *testing.T, n *Network, wrap func(*os.File) io.ReadWriteCloser) *stack.Stack {
	t.Helper()
	// The test plays the VM, so its end is non-blocking too.
	vmEnd, hostEnd, err := framePair(true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n.Attach(ctx, wrap(hostEnd))

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(256, mtu, tcpip.LinkAddress([]byte{0x5a, 0x00, 0x00, 0x00, 0x00, 0x02}))
	if err := s.CreateNIC(1, ethernet.New(link)); err != nil {
		t.Fatal(err)
	}
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(GuestIP.As4()), PrefixLen: PrefixLen},
	}, stack.AddressProperties{})
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(GatewayIP.As4()), NIC: 1}})

	go pump(ctx, link, vmEnd)
	t.Cleanup(func() { s.Close(); vmEnd.Close() })
	return s
}

func pump(ctx context.Context, link *channel.Endpoint, f *os.File) {
	go func() {
		buf := make([]byte, 65536)
		for {
			k, err := f.Read(buf)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:k]...))})
			link.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	for {
		pkt := link.ReadContext(ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		_, _ = f.Write(v.AsSlice())
		v.Release()
		pkt.DecRef()
	}
}

func allowAll(netip.Addr) error { return nil }

func dialFromGuest(t *testing.T, g *stack.Stack, addr string) (net.Conn, error) {
	t.Helper()
	ap := netip.MustParseAddrPort(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, g, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

func TestGuestReachesAllowedDestination(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	n, err := New(allowAll)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	var dialed string
	n.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = address
		return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
	}
	g := guest(t, n)
	c, err := dialFromGuest(t, g, "203.0.113.10:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
	if dialed != "203.0.113.10:443" {
		t.Errorf("forwarder dialed %q", dialed)
	}
}

func TestGuestCannotReachDeniedDestinations(t *testing.T) {
	n, err := New(DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	dialed := false
	n.Dial = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("must not dial")
	}
	g := guest(t, n)
	// (127.0.0.1 never leaves a guest at all; the policy table covers it.)
	for _, addr := range []string{"10.1.2.3:80", "169.254.169.254:80", "192.168.1.1:80", "172.17.0.1:5432", GatewayIP.String() + ":22"} {
		start := time.Now()
		if c, err := dialFromGuest(t, g, addr); err == nil {
			c.Close()
			t.Errorf("guest connected to %s", addr)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("refusing %s took %s; want an immediate reset", addr, time.Since(start))
		}
	}
	if dialed {
		t.Error("the forwarder dialed a denied destination")
	}
}

func TestDefaultPolicy(t *testing.T) {
	for addr, allowed := range map[string]bool{
		"1.1.1.1":         true,
		"93.184.216.34":   true,
		"2606:4700::1111": true,
		"10.0.0.1":        false,
		"127.0.0.1":       false,
		"169.254.169.254": false,
		"172.20.0.1":      false,
		"192.168.127.1":   false,
		"100.64.0.1":      false,
		"224.0.0.251":     false,
		"0.0.0.0":         false,
		"::1":             false,
		"fe80::1":         false,
		"fd00:ec2::254":   false,
		"::ffff:10.0.0.1": false,
		"64:ff9b::a00:1":  false,
		"255.255.255.255": false,
	} {
		err := DefaultPolicy(netip.MustParseAddr(addr))
		if (err == nil) != allowed {
			t.Errorf("DefaultPolicy(%s) = %v, want allowed=%v", addr, err, allowed)
		}
	}
	// Every address of this machine's own interfaces is refused.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			ip, _ := netip.AddrFromSlice(ipn.IP)
			if DefaultPolicy(ip.Unmap()) == nil {
				t.Errorf("host address %s allowed", ip)
			}
		}
	}
}

func TestOnHostNetwork(t *testing.T) {
	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	// A LAN numbered from public space, and a point-to-point VPN address
	// with a mask so wide it would cover the internet.
	addrs := []net.Addr{cidr("18.20.0.5/16"), cidr("44.1.2.3/1"), cidr("2001:db8::5/64")}
	for addr, want := range map[string]bool{
		"18.20.0.5":     true,  // the host itself
		"18.20.200.9":   true,  // a neighbour on its LAN
		"18.21.0.1":     false, // beyond the LAN
		"44.1.2.3":      true,  // the VPN address itself
		"44.9.9.9":      false, // not "the whole /1"
		"2001:db8::9":   true,
		"2001:db8:1::9": false,
	} {
		if got := onHostNetwork(netip.MustParseAddr(addr), addrs); got != want {
			t.Errorf("onHostNetwork(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestSMTPDenied(t *testing.T) {
	n, err := New(allowAll)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.Dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("the forwarder dialed port 25")
		return nil, errors.New("must not dial")
	}
	g := guest(t, n)
	if c, err := dialFromGuest(t, g, "203.0.113.10:25"); err == nil {
		c.Close()
		t.Error("guest connected to port 25")
	}
}

func TestTCPFlowLimit(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	n, err := New(allowAll)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.tcpSlots = make(chan struct{}, 2)
	n.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
	}
	g := guest(t, n)
	var open []net.Conn
	for i := 0; i < 2; i++ {
		c, err := dialFromGuest(t, g, "203.0.113.10:443")
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		open = append(open, c)
	}
	if c, err := dialFromGuest(t, g, "203.0.113.10:443"); err == nil {
		c.Close()
		t.Error("a third connection got through a limit of two")
	}
	// Closing one frees its slot.
	open[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := dialFromGuest(t, g, "203.0.113.10:443")
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was never freed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	open[1].Close()
}

// fullBuffer fails its first writes the way macOS does when the VM's
// receive buffer is full.
type fullBuffer struct {
	*os.File
	failures atomic.Int32
}

func (f *fullBuffer) Write(p []byte) (int, error) {
	if f.failures.Add(-1) >= 0 {
		return 0, syscall.ENOBUFS
	}
	return f.File.Write(p)
}

func TestFullBufferDropsFramesOnly(t *testing.T) {
	n, err := New(DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.Resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	g := guestVia(t, n, func(f *os.File) io.ReadWriteCloser {
		fb := &fullBuffer{File: f}
		fb.failures.Store(3)
		return fb
	})
	// The first frames (ARP replies, answers) are dropped; the guest's
	// retries must still get through.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := queryDNS(g, "registry.example.test.", dnsmessage.TypeA, 1); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the network never recovered from a full buffer")
		}
	}
}

func queryDNS(g *stack.Stack, name string, typ dnsmessage.Type, timeoutSec int) (dnsmessage.Message, error) {
	var m dnsmessage.Message
	c, err := gonet.DialUDP(g, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(GatewayIP.As4()), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		return m, err
	}
	defer c.Close()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}}}
	b, _ := q.Pack()
	_ = c.SetDeadline(time.Now().Add(time.Duration(timeoutSec) * time.Second))
	if _, err := c.Write(b); err != nil {
		return m, err
	}
	buf := make([]byte, 1500)
	k, err := c.Read(buf)
	if err != nil {
		return m, err
	}
	return m, m.Unpack(buf[:k])
}

func TestDNSSRVAndTXT(t *testing.T) {
	n, err := New(DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.LookupSRV = func(_ context.Context, name string) ([]*net.SRV, error) {
		return []*net.SRV{{Target: "shard-0.db.example.test.", Port: 27017, Priority: 0, Weight: 0}}, nil
	}
	n.LookupTXT = func(_ context.Context, name string) ([]string, error) {
		return []string{"authSource=admin&replicaSet=rs0"}, nil
	}
	g := guest(t, n)
	m, err := queryDNS(g, "_mongodb._tcp.cluster.example.test.", dnsmessage.TypeSRV, 5)
	if err != nil || len(m.Answers) != 1 {
		t.Fatalf("SRV = %+v, %v", m, err)
	}
	if srv := m.Answers[0].Body.(*dnsmessage.SRVResource); srv.Port != 27017 || srv.Target.String() != "shard-0.db.example.test." {
		t.Errorf("SRV answer = %+v", srv)
	}
	m, err = queryDNS(g, "cluster.example.test.", dnsmessage.TypeTXT, 5)
	if err != nil || len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.TXTResource).TXT[0] != "authSource=admin&replicaSet=rs0" {
		t.Errorf("TXT = %+v, %v", m, err)
	}
}

func TestDNS(t *testing.T) {
	n, err := New(DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.Resolve = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == "registry.example.test." {
			return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	g := guest(t, n)
	ask := func(name string) dnsmessage.Message {
		t.Helper()
		c, err := gonet.DialUDP(g, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(GatewayIP.As4()), Port: 53}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
			Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
		b, _ := q.Pack()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1500)
		k, err := c.Read(buf)
		if err != nil {
			t.Fatalf("asking %s: %v", name, err)
		}
		var m dnsmessage.Message
		if err := m.Unpack(buf[:k]); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := ask("registry.example.test.")
	if len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{203, 0, 113, 7} || m.ID != 7 {
		t.Errorf("answer = %+v", m)
	}
	if m := ask("missing.example.test."); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("missing name rcode = %v, want NXDOMAIN", m.RCode)
	}
}
