// Package netx gives each workspace VM its own user-mode network.
//
// The VM's network card exchanges Ethernet frames with skerry over a socket
// pair. A gVisor network stack in the runner plays the gateway: it answers
// DNS through the host's resolver and forwards the guest's TCP and UDP
// connections, each checked against a policy at connect time, after name
// resolution, so DNS tricks can't get around it. Nothing else connects the
// VM to the host: no bridge, no root, no route skerry doesn't forward.
package netx

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// Policy decides whether the guest may connect to an address.
type Policy func(netip.Addr) error

// ErrDenied is returned for destinations the policy refuses.
var ErrDenied = errors.New("destination not allowed")

var deniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",         // "this network"
		"10.0.0.0/8",        // private
		"100.64.0.0/10",     // carrier-grade NAT
		"127.0.0.0/8",       // loopback: the host's own services
		"169.254.0.0/16",    // link-local, including cloud metadata
		"172.16.0.0/12",     // private
		"192.0.0.0/24",      // IETF protocol assignments
		"192.168.0.0/16",    // private, and this VM's own network
		"198.18.0.0/15",     // benchmarking
		"224.0.0.0/4",       // multicast
		"240.0.0.0/4",       // reserved, broadcast
		"::/128", "::1/128", // unspecified, loopback
		"::ffff:0:0/96", // IPv4-mapped: checked as IPv4 instead
		"64:ff9b::/96",  // NAT64 can reach private IPv4
		"fc00::/7",      // unique local
		"fe80::/10",     // link-local
		"ff00::/8",      // multicast
		"fd00:ec2::254/128",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// DefaultPolicy allows the public internet and denies everything that
// leads back to the host or its networks: loopback, private and link-local
// ranges (cloud metadata included), and every address of the host's own
// interfaces.
func DefaultPolicy(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrDenied)
	}
	for _, p := range deniedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s is in %s", ErrDenied, addr, p)
		}
	}
	if hostAddress(addr) {
		return fmt.Errorf("%w: %s belongs to this machine", ErrDenied, addr)
	}
	return nil
}

// hostAddress reports whether addr is one of the host's own addresses or
// on a network the host is directly attached to, which covers local
// networks numbered from public space. It is checked on every connection,
// so new interfaces (VPNs) count at once.
func hostAddress(addr netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // can't tell: fail closed
	}
	return onHostNetwork(addr, addrs)
}

func onHostNetwork(addr netip.Addr, addrs []net.Addr) bool {
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip == addr {
			return true
		}
		ones, bits := n.Mask.Size()
		if bits == 128 && ip.Is4() {
			ones -= 96
		}
		// A very wide mask (a misconfigured or point-to-point interface)
		// would deny the whole internet; for those, only the address counts.
		if ones >= minSubnetBits(ip) {
			if p, err := ip.Prefix(ones); err == nil && p.Contains(addr) {
				return true
			}
		}
	}
	return false
}

func minSubnetBits(ip netip.Addr) int {
	if ip.Is4() {
		return 8
	}
	return 32
}
