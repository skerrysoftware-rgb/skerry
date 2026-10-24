//go:build linux

package agent

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// relayPort makes a service answer on another loopback port too: the one
// the project's env files use. A connection keeps its kind: one forwarded
// from the host (from 127.0.0.2) reaches the service from 127.0.0.2 and
// must authenticate as before; the VM's own reach it as the VM's own.
func relayPort(a PortAlias) {
	to := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(a.To)))
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(int(a.Port))))
		if err != nil {
			logger.Printf("port %d for %d: %v", a.Port, a.To, err)
			continue
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				go relay(c, to)
			}
		}()
	}
	select {}
}

func relay(in net.Conn, to string) {
	defer in.Close()
	dialer := net.Dialer{Timeout: 5 * time.Second}
	if ip := in.RemoteAddr().(*net.TCPAddr).IP; !ip.Equal(net.IPv4(127, 0, 0, 1)) && !ip.Equal(net.IPv6loopback) {
		dialer.LocalAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}
	}
	out, err := dialer.Dial("tcp", to)
	if err != nil {
		return
	}
	defer out.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go pipe(out, in)
	go pipe(in, out)
	wg.Wait()
}
