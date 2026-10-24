package vmm

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The web forward makes requests look as they do in the VM: an app that
// checks Host (as many dev servers do) accepts them.
func TestWebForwardPresentsTheAppsHost(t *testing.T) {
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	guestPort := uint16(app.Addr().(*net.TCPAddr).Port)
	want := "localhost:" + strings.TrimPrefix(app.Addr().String(), "127.0.0.1:")
	go http.Serve(app, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != want {
			http.Error(w, "misdirected: "+r.Host, http.StatusMisdirectedRequest)
			return
		}
		if r.URL.Path == "/go" {
			http.Redirect(w, r, "http://"+want+"/there", http.StatusFound)
			return
		}
		if r.URL.Path == "/rel" {
			w.Header().Set("Location", "//"+want+"/there")
			w.WriteHeader(http.StatusFound)
			return
		}
		io.WriteString(w, "origin="+r.Header.Get("Origin"))
	}))
	d := &guestDialer{act: newActivity(), open: map[net.Conn]bool{}, own: "ws.localhost"}
	d.dialFunc = func(port uint16) (net.Conn, error) { return net.Dial("tcp", app.Addr().String()) }
	l, err := d.listenWeb(0, guestPort)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	outer := l.Addr().String()

	req, _ := http.NewRequest("GET", "http://"+outer+"/", nil)
	req.Header.Set("Origin", "http://"+outer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "origin=http://"+want {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
	// A .localhost name the app serves reaches it as that name.
	req, _ = http.NewRequest("GET", "http://"+outer+"/", nil)
	req.Host = "docs.app.localhost:" + strings.Split(outer, ":")[1]
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest || !strings.Contains(string(body), "docs.app.localhost:"+strconv.Itoa(int(guestPort))) {
		t.Errorf("named host got %d %q", resp.StatusCode, body)
	}
	// The workspace's own name reads as localhost, and a protocol-relative
	// redirect comes back through the forward too.
	port := strings.Split(outer, ":")[1]
	req, _ = http.NewRequest("GET", "http://"+outer+"/rel", nil)
	req.Host = "ws.localhost:" + port
	if resp, err = http.DefaultTransport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "//ws.localhost:"+port+"/there" {
		t.Errorf("own name: %d, Location %q", resp.StatusCode, loc)
	}
	// A page on another name, rebound to 127.0.0.1, isn't let through.
	req, _ = http.NewRequest("GET", "http://"+outer+"/", nil)
	req.Host = "evil.example:" + strings.Split(outer, ":")[1]
	req.Header.Set("Origin", "http://"+req.Host)
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("rebound host got %d", resp.StatusCode)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noRedirect.Get("http://" + outer + "/go")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "http://"+outer+"/there" {
		t.Errorf("redirect to %q, want it back through the forward", loc)
	}
}
