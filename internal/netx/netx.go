package netx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
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
	"gvisor.dev/gvisor/pkg/waiter"
)

// Addresses of every workspace network. Each workspace has its own stack,
// so they never meet.
var (
	GatewayIP = netip.MustParseAddr("192.168.127.1")
	GuestIP   = netip.MustParseAddr("192.168.127.2")
	PrefixLen = 24
	// GatewayMAC is the stack's own link address; locally administered.
	GatewayMAC = net.HardwareAddr{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xdd}
)

const (
	nicID = 1
	mtu   = 1500
	// dialTimeout bounds connecting out on the guest's behalf.
	dialTimeout = 10 * time.Second
	// udpIdle closes a UDP flow nobody has used for this long.
	udpIdle = 30 * time.Second
	// dnsWorkers answer DNS queries in parallel; dnsQueue more can wait.
	dnsWorkers     = 8
	dnsQueue       = 64
	maxDNSResponse = 1232
	// maxFlows bounds forwarded TCP connections being set up.
	maxFlows = 256
	// maxTCPFlows and maxUDPFlows bound established flows, so a guest
	// can't exhaust the host's ports or file descriptors. (DNS has its own
	// socket and queue.)
	maxTCPFlows = 512
	maxUDPFlows = 128
)

// deniedPorts are refused whatever the destination: SMTP, so code in a VM
// can't send mail as the user's network.
var deniedPorts = map[uint16]bool{25: true}

// Network is one workspace's user-mode network.
type Network struct {
	stack  *stack.Stack
	link   *channel.Endpoint
	policy Policy
	// Dial connects out; tests replace it.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// Resolve looks up a name; tests replace it. Defaults to the host's
	// resolver, so corporate and VPN DNS keep working.
	Resolve func(ctx context.Context, network, host string) ([]netip.Addr, error)
	// LookupSRV and LookupTXT answer SRV and TXT queries (mongodb+srv://
	// and similar); tests replace them.
	LookupSRV func(ctx context.Context, name string) ([]*net.SRV, error)
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	Log       *log.Logger
	// dns is the gateway's DNS socket; queries wait in dnsJobs for a
	// worker, and are dropped when too many are waiting.
	dns     *gonet.UDPConn
	dnsJobs chan dnsJob
	dropped atomic.Int64 // frames dropped because the VM wasn't reading
	// tcpSlots and udpSlots hold one token per established flow.
	tcpSlots, udpSlots chan struct{}
	wg                 sync.WaitGroup
	mu                 sync.Mutex
	conns              []io.Closer
}

// New builds a network that enforces policy.
func New(policy Policy) (*Network, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(1024, mtu, tcpip.LinkAddress(GatewayMAC))
	if err := s.CreateNIC(nicID, ethernet.New(link)); err != nil {
		return nil, fmt.Errorf("create NIC: %s", err)
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4(GatewayIP.As4()).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("gateway address: %s", err)
	}
	// Accept the guest's packets for any destination; the forwarders and
	// the policy decide what happens to them.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, fmt.Errorf("promiscuous: %s", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, fmt.Errorf("spoofing: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	n := &Network{
		stack: s, link: link, policy: policy,
		Dial:    (&net.Dialer{Timeout: dialTimeout}).DialContext,
		Resolve: net.DefaultResolver.LookupNetIP,
		LookupSRV: func(ctx context.Context, name string) ([]*net.SRV, error) {
			_, srvs, err := net.DefaultResolver.LookupSRV(ctx, "", "", name)
			return srvs, err
		},
		LookupTXT: net.DefaultResolver.LookupTXT,
		Log:       log.New(io.Discard, "", 0),
		dnsJobs:   make(chan dnsJob, dnsQueue),
		tcpSlots:  make(chan struct{}, maxTCPFlows),
		udpSlots:  make(chan struct{}, maxUDPFlows),
	}
	tcpFwd := tcp.NewForwarder(s, 0, maxFlows, n.forwardTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, n.forwardUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	// One socket serves DNS for every guest port; bound endpoints take
	// their packets before the forwarder sees them.
	dns, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(GatewayIP.As4()), Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("dns socket: %w", err)
	}
	n.dns = dns
	n.wg.Add(1 + dnsWorkers)
	go n.readDNS()
	for i := 0; i < dnsWorkers; i++ {
		go n.dnsWorker()
	}
	return n, nil
}

// FramePair returns two ends of a datagram socket pair for Ethernet frames:
// one for the VM's network card, one for Attach. The caller must keep vm
// referenced for as long as the VM runs: if it were garbage collected, Go
// would close the descriptor underneath the VM.
func FramePair() (vm, host *os.File, err error) { return framePair(false) }

func framePair(nonblockVM bool) (vm, host *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd) // never leak into child processes
		// Apple recommends a receive buffer about four times the send one.
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	}
	// Skerry's end is non-blocking, so Go's poller manages it and closing
	// it wakes a pending read. The VM's end stays as the framework expects.
	nonblock := []int{fds[1]}
	if nonblockVM {
		nonblock = append(nonblock, fds[0])
	}
	for _, fd := range nonblock {
		if err := unix.SetNonblock(fd, true); err != nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return nil, nil, err
		}
	}
	return os.NewFile(uintptr(fds[0]), "vm-net"), os.NewFile(uintptr(fds[1]), "host-net"), nil
}

// Attach moves frames between the VM (through conn) and the stack until ctx
// ends or conn fails.
func (n *Network) Attach(ctx context.Context, conn io.ReadWriteCloser) {
	// As a socket rather than a file, a zero-length datagram isn't taken
	// for end of file.
	if f, ok := conn.(*os.File); ok {
		if c, err := net.FileConn(f); err == nil {
			f.Close()
			conn = c
		}
	}
	n.mu.Lock()
	n.conns = append(n.conns, conn)
	n.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	n.wg.Add(2)
	go func() { // VM → stack
		defer n.wg.Done()
		defer cancel()
		buf := make([]byte, 65536)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					n.Log.Printf("network stopped: reading from the VM: %v", err)
				}
				return
			}
			if k < header.EthernetMinimumSize {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(append([]byte(nil), buf[:k]...)),
			})
			n.link.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	go func() { // stack → VM
		defer n.wg.Done()
		defer conn.Close()
		for {
			pkt := n.link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			_, err := conn.Write(view.AsSlice())
			view.Release()
			pkt.DecRef()
			switch {
			case err == nil:
			case errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.EAGAIN):
				// The VM isn't reading fast enough and its buffer is full.
				// Drop the frame, as a busy network would; TCP resends.
				if n.dropped.Add(1)%10000 == 1 {
					n.Log.Printf("dropping frames: the VM's receive buffer is full (%d so far)", n.dropped.Load())
				}
			default:
				if !errors.Is(err, net.ErrClosed) {
					n.Log.Printf("network stopped: writing to the VM: %v", err)
				}
				return
			}
		}
	}()
}

// Close shuts the stack down and waits for the frame pumps to stop.
func (n *Network) Close() {
	n.dns.Close()
	n.link.Close()
	n.mu.Lock()
	for _, c := range n.conns {
		_ = c.Close()
	}
	n.mu.Unlock()
	n.stack.Close()
	n.wg.Wait()
}

// forwardTCP connects the guest's TCP connection to its destination, if the
// policy allows it.
func (n *Network) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst, ok := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	if !ok {
		r.Complete(true)
		return
	}
	if dst == GatewayIP {
		r.Complete(true) // nothing listens on the gateway
		return
	}
	if err := n.allowed(dst, id.LocalPort); err != nil {
		n.Log.Printf("tcp to %s:%d refused: %v", dst, id.LocalPort, err)
		r.Complete(true)
		return
	}
	if !acquire(n.tcpSlots) {
		n.Log.Printf("tcp to %s:%d refused: %d connections open", dst, id.LocalPort, maxTCPFlows)
		r.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	out, err := n.Dial(ctx, "tcp", netip.AddrPortFrom(dst, id.LocalPort).String())
	cancel()
	if err != nil {
		<-n.tcpSlots
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		<-n.tcpSlots
		out.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	in := gonet.NewTCPConn(&wq, ep)
	go func() {
		defer func() { <-n.tcpSlots }()
		splice(in, out)
	}()
}

// allowed applies the port rules and the policy.
func (n *Network) allowed(dst netip.Addr, port uint16) error {
	if deniedPorts[port] {
		return fmt.Errorf("%w: port %d", ErrDenied, port)
	}
	return n.policy(dst)
}

// acquire takes a slot without waiting; false means all are taken.
func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// forwardUDP handles DNS on the gateway itself and relays other UDP flows
// the policy allows.
func (n *Network) forwardUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst, ok := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	if !ok {
		return false
	}
	var wq waiter.Queue
	if dst == GatewayIP {
		return false // DNS has its own socket; nothing else listens here
	}
	if err := n.allowed(dst, id.LocalPort); err != nil {
		n.Log.Printf("udp to %s:%d refused: %v", dst, id.LocalPort, err)
		return false
	}
	// Every flow holds a slot until it goes idle.
	if !acquire(n.udpSlots) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	out, err := n.Dial(ctx, "udp", netip.AddrPortFrom(dst, id.LocalPort).String())
	cancel()
	if err != nil {
		<-n.udpSlots
		return false
	}
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		<-n.udpSlots
		out.Close()
		return false
	}
	go func() {
		defer func() { <-n.udpSlots }()
		relayUDP(gonet.NewUDPConn(&wq, ep), out)
	}()
	return true
}

type dnsJob struct {
	query []byte
	from  net.Addr
}

// readDNS queues the guest's DNS queries for the workers.
func (n *Network) readDNS() {
	defer n.wg.Done()
	defer close(n.dnsJobs)
	buf := make([]byte, 1500)
	for {
		k, from, err := n.dns.ReadFrom(buf)
		if err != nil {
			return
		}
		select {
		case n.dnsJobs <- dnsJob{append([]byte(nil), buf[:k]...), from}:
		default: // too many waiting; the guest's resolver retries
		}
	}
}

// dnsWorker answers queued queries through the host resolver.
func (n *Network) dnsWorker() {
	defer n.wg.Done()
	for job := range n.dnsJobs {
		resp, err := n.answer(job.query)
		if err == nil {
			_, _ = n.dns.WriteTo(resp, job.from)
		}
	}
}

func (n *Network) answer(query []byte) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	resp := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true, RecursionDesired: h.RecursionDesired},
		Questions: []dnsmessage.Question{q},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name := q.Name.String()
	var lookupErr error
	if q.Class == dnsmessage.ClassINET {
		switch q.Type {
		case dnsmessage.TypeA:
			var addrs []netip.Addr
			addrs, lookupErr = n.Resolve(ctx, "ip4", name)
			for _, a := range addrs {
				if a.Unmap().Is4() {
					resp.Answers = append(resp.Answers, dnsmessage.Resource{
						Header: answerHeader(q),
						Body:   &dnsmessage.AResource{A: a.Unmap().As4()},
					})
				}
			}
		case dnsmessage.TypeSRV:
			var srvs []*net.SRV
			srvs, lookupErr = n.LookupSRV(ctx, name)
			for _, srv := range srvs {
				target, err := dnsmessage.NewName(srv.Target)
				if err != nil {
					continue
				}
				resp.Answers = append(resp.Answers, dnsmessage.Resource{
					Header: answerHeader(q),
					Body:   &dnsmessage.SRVResource{Priority: srv.Priority, Weight: srv.Weight, Port: srv.Port, Target: target},
				})
			}
		case dnsmessage.TypeTXT:
			var txts []string
			txts, lookupErr = n.LookupTXT(ctx, name)
			for _, txt := range txts {
				resp.Answers = append(resp.Answers, dnsmessage.Resource{
					Header: answerHeader(q),
					Body:   &dnsmessage.TXTResource{TXT: splitTXT(txt)},
				})
			}
		}
		// Other types (AAAA included) get an empty answer, so the guest
		// falls back to IPv4.
	}
	var dnsErr *net.DNSError
	switch {
	case errors.As(lookupErr, &dnsErr) && dnsErr.IsNotFound:
		resp.RCode = dnsmessage.RCodeNameError
	case lookupErr != nil:
		resp.RCode = dnsmessage.RCodeServerFailure
	}
	out, err := resp.Pack()
	// Too big for one datagram: send as many answers as fit. (Marking it
	// truncated would send the resolver to TCP, which the gateway doesn't
	// serve.)
	if err == nil && len(out) > maxDNSResponse {
		// Cut in proportion first, so a huge answer costs a few packs.
		keep := len(resp.Answers) * maxDNSResponse / len(out)
		resp.Answers = resp.Answers[:keep]
		out, err = resp.Pack()
	}
	for err == nil && len(out) > maxDNSResponse && len(resp.Answers) > 0 {
		resp.Answers = resp.Answers[:len(resp.Answers)-1]
		out, err = resp.Pack()
	}
	return out, err
}

func answerHeader(q dnsmessage.Question) dnsmessage.ResourceHeader {
	return dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: q.Class, TTL: 30}
}

// splitTXT cuts a TXT record into the 255-byte strings DNS carries.
func splitTXT(s string) []string {
	var out []string
	for len(s) > 255 {
		out = append(out, s[:255])
		s = s[255:]
	}
	return append(out, s)
}

// splice copies both ways, passing half-closes through, until both sides end.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			_ = cw.CloseWrite()
		} else {
			// A reset or other error ends the whole connection, so the
			// other direction doesn't wait on an idle peer forever.
			_ = a.Close()
			_ = b.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// relayUDP copies datagrams both ways until the flow is idle.
func relayUDP(guest *gonet.UDPConn, out net.Conn) {
	defer guest.Close()
	defer out.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 65536)
		for {
			_ = src.SetReadDeadline(time.Now().Add(udpIdle))
			k, err := src.Read(buf)
			if err != nil {
				break
			}
			if _, err := dst.Write(buf[:k]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go cp(out, guest)
	go cp(guest, out)
	<-done
}
