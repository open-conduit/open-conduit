package gateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upstream is a local HTTP server standing for every outside host: the
// gateway's dialer sends each allowed host there.
func upstream(t *testing.T) (dial func(context.Context, string, string) (net.Conn, error), dialed *[]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "upstream %s %s", r.Host, r.URL.Path) }))
	}()
	var got []string
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		got = append(got, addr)
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}, &got
}

// resolver is a stand-in DNS: names in m resolve as listed, any other to
// a public documentation address, 203.0.113.7.
func resolver(m map[string][]string) func(context.Context, string) ([]net.IP, error) {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		// A lookup, like a real one, takes a moment and fails once its
		// context is cancelled.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		list, ok := m[host]
		if !ok {
			list = []string{"203.0.113.7"}
		}
		var out []net.IP
		for _, a := range list {
			out = append(out, net.ParseIP(a))
		}
		return out, nil
	}
}

func start(t *testing.T, o Options) *Gateway {
	t.Helper()
	o.Listen = "127.0.0.1:0"
	if o.Resolve == nil {
		o.Resolve = resolver(nil)
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	g, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}

// connect sends CONNECT host through the proxy, then a GET through the
// tunnel, and returns the CONNECT status line and what came back.
func connect(t *testing.T, proxy, host string) (string, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	// All of it at once, then a half-close, as nc sends it: the tunnel
	// must outlast the request.
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nGET /tunnelled HTTP/1.0\r\nHost: %s\r\n\r\n", host, host, host)
	_ = c.(*net.TCPConn).CloseWrite()
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, " 200 ") {
		return strings.TrimSpace(status), ""
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	body, _ := io.ReadAll(br)
	return strings.TrimSpace(status), string(body)
}

// TestProxyAllowlist: CONNECT and absolute-form HTTP reach only allowed
// hosts, a port-pinned entry only its port, and a denied host gets 403
// with nothing dialled.
func TestProxyAllowlist(t *testing.T) {
	dial, dialed := upstream(t)
	var log bytes.Buffer
	g := start(t, Options{Allow: []string{"api.example.test", "*.allowed.test", "pinned.test:8443"}, Dial: dial, Log: &log})
	for _, c := range []struct {
		host string
		ok   bool
	}{
		{"api.example.test:443", true},
		{"deep.sub.allowed.test:443", true},
		{"pinned.test:8443", true},
		{"pinned.test:443", false},
		{"allowed.test:443", false},
		{"evil.test:443", false},
		{"api.example.test.evil.test:443", false},
		{"127.0.0.1:443", false},
	} {
		status, body := connect(t, g.ProxyAddr(), c.host)
		if c.ok && (!strings.Contains(status, "200") || !strings.Contains(body, "upstream "+c.host+" /tunnelled")) {
			t.Errorf("CONNECT %s: %q %q, want tunnelled", c.host, status, body)
		}
		if !c.ok && !strings.Contains(status, "403") {
			t.Errorf("CONNECT %s: %q, want 403", c.host, status)
		}
	}
	// Only the three allowed CONNECTs dialled, each at its checked address.
	if len(*dialed) != 3 {
		t.Errorf("dialled %v, want the three allowed hosts only", *dialed)
	}
	for _, d := range *dialed {
		if d != "203.0.113.7:443" && d != "203.0.113.7:8443" {
			t.Errorf("dialled %s, not a checked address", d)
		}
	}
	if !strings.Contains(log.String(), "refused CONNECT evil.test:443") {
		t.Errorf("no refusal logged: %q", log.String())
	}

	proxy := func(target string) (*http.Response, string) {
		t.Helper()
		u, _ := http.NewRequest("GET", "http://"+g.ProxyAddr(), nil)
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u.URL)}, Timeout: 10 * time.Second}
		resp, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := proxy("http://api.example.test/plain"); resp.StatusCode != 200 || body != "upstream api.example.test /plain" {
		t.Errorf("an allowed http request: %d %q", resp.StatusCode, body)
	}
	if resp, _ := proxy("http://evil.test/plain"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a denied http request: %d, want 403", resp.StatusCode)
	}
	// A request that isn't for a proxy is refused.
	resp, err := http.Get("http://" + g.ProxyAddr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an origin-form request: %d, want 400", resp.StatusCode)
	}
}

// toolbelt is a stand-in for the execution's toolbelt socket.
func toolbelt(t *testing.T, ln net.Listener) {
	t.Helper()
	t.Cleanup(func() { ln.Close() })
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "toolbelt %s %s", r.Method, r.URL.Path)
		}))
	}()
}

func call(t *testing.T, socket string) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	resp, err := client.Post("http://localhost/v1/toolbelt/call", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// shortDir is a directory whose socket paths fit the platform limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ocgw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestForward: the forward socket reaches the toolbelt's socket, and in
// tcp mode the port its file names, read again for each connection.
func TestForward(t *testing.T) {
	t.Run("unix", func(t *testing.T) {
		dir := shortDir(t)
		ln, err := net.Listen("unix", filepath.Join(dir, "toolbelt.sock"))
		if err != nil {
			t.Fatal(err)
		}
		toolbelt(t, ln)
		g := start(t, Options{Socket: filepath.Join(dir, "fwd.sock"), Forward: "unix:" + filepath.Join(dir, "toolbelt.sock")})
		if got := call(t, g.o.Socket); got != "toolbelt POST /v1/toolbelt/call" {
			t.Errorf("through the forward: %q", got)
		}
		if fi, err := os.Stat(g.o.Socket); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("forward socket %v %v, want 0600", fi, err)
		}
	})
	t.Run("tcp", func(t *testing.T) {
		dir := shortDir(t)
		port := filepath.Join(dir, "toolbelt.port")
		g := start(t, Options{Socket: filepath.Join(dir, "fwd.sock"), Forward: "tcp:" + port, ForwardHost: "127.0.0.1"})
		for i := range 2 {
			// A restarted daemon serves a new port and rewrites the file.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			toolbelt(t, ln)
			_, p, _ := net.SplitHostPort(ln.Addr().String())
			if err := os.WriteFile(port, []byte(p+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := call(t, g.o.Socket); got != "toolbelt POST /v1/toolbelt/call" {
				t.Errorf("pass %d through the forward: %q", i, got)
			}
			ln.Close()
		}
	})
	if _, err := Start(Options{Listen: "127.0.0.1:0", Socket: "/tmp/x.sock", Forward: "http://x"}); err == nil {
		t.Error("a forward that is neither unix nor tcp was accepted")
	}
}

func TestParse(t *testing.T) {
	o, err := Parse([]string{"--listen", ":3128", "--allow", "a.test, b.test", "--allow", "c.test:443", "--socket", "/s", "--forward", "unix:/t"})
	if err != nil || o.Listen != ":3128" || strings.Join(o.Allow, " ") != "a.test b.test c.test:443" || o.Socket != "/s" || o.Forward != "unix:/t" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := Parse([]string{"--bogus", "x"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
	if _, err := Parse([]string{"--allow"}); err == nil {
		t.Error("a flag without a value was accepted")
	}
	p, err := ParseProbe([]string{"--alias", "gateway", "--outside", "example.com", "--unreachable", "a:1,b:2"})
	if err != nil || p.Alias != "gateway" || p.Outside != "example.com" || len(p.Unreachable) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
}

// TestProbe: each check passes and fails as it should, against local
// names and listeners only.
func TestProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := closed.Addr().String()
	closed.Close()
	var out bytes.Buffer
	if !Probe(context.Background(), ProbeOptions{Alias: "localhost", Outside: "open-conduit-probe.invalid", Unreachable: []string{dead}, Timeout: time.Second}, &out) {
		t.Errorf("a clean probe failed:\n%s", out.String())
	}
	out.Reset()
	if Probe(context.Background(), ProbeOptions{Outside: "localhost", Unreachable: []string{ln.Addr().String()}, Timeout: time.Second}, &out) {
		t.Errorf("a probe that reached a listener and resolved a name passed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "fail no outside DNS") || !strings.Contains(out.String(), "fail can't reach "+ln.Addr().String()) {
		t.Errorf("probe output:\n%s", out.String())
	}
}

// TestProxyRefusesInwardNames: an allowed name that resolves to loopback,
// link-local (a metadata service), private, CGNAT, "this network" or a
// denied address gets 403 with nothing dialled, by CONNECT and by HTTP,
// as does one with any such address among public ones. A listed literal
// address is reached as it is.
func TestProxyRefusesInwardNames(t *testing.T) {
	dial, dialed := upstream(t)
	var log bytes.Buffer
	g := start(t, Options{Allow: []string{"*.allowed.test", "10.1.2.3"}, Dial: dial, Log: &log, Deny: []net.IP{net.ParseIP("198.51.100.1")},
		Resolve: resolver(map[string][]string{
			"loop.allowed.test":   {"127.0.0.1"},
			"meta.allowed.test":   {"169.254.169.254"},
			"lan.allowed.test":    {"192.168.1.20"},
			"ula.allowed.test":    {"fd00::1"},
			"v6loop.allowed.test": {"::1"},
			"cgnat.allowed.test":  {"100.100.1.1"},
			"orb.allowed.test":    {"0.250.250.254"},
			"denied.allowed.test": {"198.51.100.1"},
			"mixed.allowed.test":  {"203.0.113.9", "10.0.0.5"},
			"public.allowed.test": {"203.0.113.9"},
		})})
	for _, name := range []string{"loop", "meta", "lan", "ula", "v6loop", "cgnat", "orb", "denied", "mixed"} {
		host := name + ".allowed.test"
		if status, _ := connect(t, g.ProxyAddr(), host+":443"); !strings.Contains(status, "403") {
			t.Errorf("CONNECT %s: %q, want 403", host, status)
		}
	}
	u, _ := http.NewRequest("GET", "http://"+g.ProxyAddr(), nil)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u.URL)}, Timeout: 10 * time.Second}
	resp, err := client.Get("http://meta.allowed.test/latest/meta-data/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an http request to a name resolving to the metadata address: %d, want 403", resp.StatusCode)
	}
	if len(*dialed) != 0 {
		t.Errorf("dialled %v for names that resolve inward", *dialed)
	}
	if !strings.Contains(log.String(), "refused CONNECT meta.allowed.test:443") {
		t.Errorf("no refusal logged: %q", log.String())
	}
	if status, body := connect(t, g.ProxyAddr(), "public.allowed.test:443"); !strings.Contains(status, "200") || !strings.Contains(body, "upstream") {
		t.Errorf("a public allowed name: %q %q", status, body)
	}
	if status, _ := connect(t, g.ProxyAddr(), "10.1.2.3:443"); !strings.Contains(status, "200") {
		t.Errorf("a listed literal private address: %q, want reached", status)
	}
	if got := (*dialed)[len(*dialed)-2:]; got[0] != "203.0.113.9:443" || got[1] != "10.1.2.3:443" {
		t.Errorf("dialled %v, want the checked addresses", got)
	}
}

func TestDefaultRoutes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "route")
	table := "Iface\tDestination\tGateway \tFlags\n" +
		"eth1\t00000000\t010012AC\t0003\n" +
		"eth0\t000012AC\t00000000\t0001\n"
	if err := os.WriteFile(file, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultRoutes(file); len(got) != 1 || !got[0].Equal(net.ParseIP("172.18.0.1")) {
		t.Errorf("default routes %v, want 172.18.0.1", got)
	}
	if defaultRoutes(file+".missing") != nil {
		t.Error("a missing table gave routes")
	}
}
