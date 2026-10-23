package netx

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

// DNS queries come from the guest.
func FuzzAnswer(f *testing.F) {
	f.Add([]byte{0, 7, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 0, 1, 0, 1})
	f.Add([]byte{})
	n := &Network{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.1")}, nil
		},
		LookupSRV: func(context.Context, string) ([]*net.SRV, error) { return nil, nil },
		LookupTXT: func(context.Context, string) ([]string, error) { return []string{string(make([]byte, 600))}, nil },
	}
	f.Fuzz(func(t *testing.T, query []byte) {
		resp, err := n.answer(query)
		if err == nil && len(resp) > maxDNSResponse {
			t.Errorf("answer of %d bytes exceeds the limit", len(resp))
		}
	})
}
