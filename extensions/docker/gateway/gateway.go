// Package gateway is the program in the Docker provider's gateway image.
// One gateway container runs beside each launch's container, on the
// execution's internal network (as "gateway") and on an egress network of
// its own, and does two things:
//
//   - an allowlisting HTTP proxy: CONNECT and absolute-form HTTP requests
//     reach only the hosts the grant's network.allow lists, and anything
//     else gets 403. A name is resolved here, and refused if any address
//     it resolves to is the host's, the gateway's own networks', or
//     loopback, link-local or private, unless the entry is that literal
//     address; the proxy dials the addresses it checked;
//   - the toolbelt forward: it serves a unix socket in a volume the two
//     containers share and copies each connection to the execution's
//     toolbelt on the host, either its socket (mounted, on Linux) or a
//     loopback port the host names in a file (on macOS, where a host socket
//     can't be reached from the VM).
//
// Probe is doctor's preflight: run on a preflight internal network, it
// checks what an agent container there can and can't reach.
package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// Options are a gateway's settings, from its argv (Parse).
type Options struct {
	// Listen is the proxy's address (":3128").
	Listen string
	// Allow are the hosts (host, host:port or *.domain) the proxy reaches.
	Allow []string
	// Socket is the unix socket the forward serves; empty serves none.
	Socket string
	// Forward is where each toolbelt connection goes: "unix:<socket>" or
	// "tcp:<file>", a file holding the host port, read for each connection
	// so a restarted daemon's new port is picked up.
	Forward string
	// ForwardHost is the host a tcp forward dials (host.docker.internal).
	ForwardHost string
	// Dial reaches a checked address (net.Dialer's when nil).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Resolve looks a name up (the system resolver's when nil).
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// Deny are more addresses no name may resolve to. Start adds the
	// addresses of host.docker.internal and of the gateway's default route.
	Deny []net.IP
	// Log gets one line for each refused request (stderr when nil).
	Log io.Writer
}

// Gateway is a running gateway.
type Gateway struct {
	o       Options
	proxy   net.Listener
	forward net.Listener
	srv     *http.Server
	wg      sync.WaitGroup
}

// Start listens and serves until Close.
func Start(o Options) (*Gateway, error) {
	if o.Listen == "" {
		o.Listen = ":3128"
	}
	if o.ForwardHost == "" {
		o.ForwardHost = "host.docker.internal"
	}
	if o.Dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second}
		o.Dial = d.DialContext
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	if o.Resolve == nil {
		o.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if ips, err := o.Resolve(ctx, "host.docker.internal"); err == nil {
			o.Deny = append(o.Deny, ips...)
		}
		cancel()
		o.Deny = append(o.Deny, defaultRoutes("/proc/net/route")...)
	}
	g := &Gateway{o: o}
	l, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return nil, err
	}
	g.proxy = l
	g.srv = &http.Server{Handler: http.HandlerFunc(g.serveProxy), ReadHeaderTimeout: 30 * time.Second}
	g.wg.Add(1)
	go func() { defer g.wg.Done(); _ = g.srv.Serve(l) }()
	if o.Socket != "" {
		if !strings.HasPrefix(o.Forward, "unix:") && !strings.HasPrefix(o.Forward, "tcp:") {
			g.Close()
			return nil, fmt.Errorf("forward %q is not unix:<socket> or tcp:<port file>", o.Forward)
		}
		_ = os.Remove(o.Socket)
		f, err := net.Listen("unix", o.Socket)
		if err != nil {
			g.Close()
			return nil, err
		}
		_ = os.Chmod(o.Socket, 0o600)
		g.forward = f
		g.wg.Add(1)
		go func() { defer g.wg.Done(); g.serveForward(f) }()
	}
	return g, nil
}

// ProxyAddr is the proxy's listening address.
func (g *Gateway) ProxyAddr() string { return g.proxy.Addr().String() }

// Close stops serving.
func (g *Gateway) Close() {
	if g.srv != nil {
		_ = g.srv.Close()
	}
	if g.forward != nil {
		_ = g.forward.Close()
	}
	g.wg.Wait()
}

func (g *Gateway) refuse(w http.ResponseWriter, code int, what, host string) {
	fmt.Fprintf(g.o.Log, "gateway: refused %s %s\n", what, host)
	http.Error(w, fmt.Sprintf("open-conduit gateway: %s is not in this binding's network.allow", host), code)
}

// withPort is host with the scheme's port when it names none, so an
// allow entry with a port is matched against the port actually used.
func withPort(host, port string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

// allowed reports whether host:port may be reached.
func (g *Gateway) allowed(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || host == "" || port == "" {
		return false
	}
	return extension.HostAllowed(host+":"+port, g.o.Allow)
}

// errInward is a name that resolves where no allowed name may lead.
var errInward = errors.New("resolves to an address the gateway won't reach")

// inward ranges a name may never resolve to: beyond loopback, link-local,
// private (RFC 1918, ULA), unspecified and multicast, "this network" (where
// OrbStack puts host.docker.internal) and shared address space (CGNAT).
var inward = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func (g *Gateway) inward(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	for _, n := range inward {
		if n.Contains(ip) {
			return true
		}
	}
	for _, d := range g.o.Deny {
		if d.Equal(ip) {
			return true
		}
	}
	return false
}

// addrs are the checked addresses an allowed host:port is dialled at. A
// literal address passed the allowlist only by being listed, so it is
// dialled as it is. A name is resolved, and refused (errInward) if any of
// its addresses is inward, so a wildcard entry or a rebinding DNS answer
// can't lead to the host, the LAN or a metadata service.
func (g *Gateway) addrs(ctx context.Context, hostport string) ([]string, error) {
	host, port, _ := net.SplitHostPort(hostport)
	if net.ParseIP(host) != nil {
		return []string{hostport}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := g.o.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no addresses", host)
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if g.inward(ip) {
			return nil, fmt.Errorf("%s %w (%s)", host, errInward, ip)
		}
		out = append(out, net.JoinHostPort(ip.String(), port))
	}
	return out, nil
}

// dial reaches the first of the checked addresses that answers.
func (g *Gateway) dial(ctx context.Context, network string, addrs []string) (net.Conn, error) {
	var errs []error
	for _, a := range addrs {
		c, err := g.o.Dial(ctx, network, a)
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// check is the allowlist and the addresses: it writes the refusal and
// returns false when host:port may not be reached.
func (g *Gateway) check(ctx context.Context, w http.ResponseWriter, what, host string) ([]string, bool) {
	if !g.allowed(host) {
		g.refuse(w, http.StatusForbidden, what, host)
		return nil, false
	}
	addrs, err := g.addrs(ctx, host)
	switch {
	case errors.Is(err, errInward):
		fmt.Fprintf(g.o.Log, "gateway: refused %s %s: %v\n", what, host, err)
		http.Error(w, "open-conduit gateway: "+err.Error(), http.StatusForbidden)
		return nil, false
	case err != nil:
		http.Error(w, "open-conduit gateway: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	return addrs, true
}

// defaultRoutes are the gateways of the default routes in a
// /proc/net/route table: the gateway's egress network's bridge.
func defaultRoutes(file string) []net.IP {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		out = append(out, net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)))
	}
	return out
}

func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		host := withPort(r.Host, "443")
		// Not the request's context, as for the tunnel: a client that has
		// sent all it will half-closes, and the server cancels it.
		addrs, ok := g.check(context.WithoutCancel(r.Context()), w, "CONNECT", host)
		if !ok {
			return
		}
		g.tunnel(w, r, addrs)
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		host := withPort(r.URL.Host, "80")
		addrs, ok := g.check(r.Context(), w, "GET", host)
		if !ok {
			return
		}
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Host = host
				pr.Out.Host = r.URL.Host
			},
			Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				// Dial the addresses checked, never the name again.
				return g.dial(ctx, network, addrs)
			}, Proxy: nil, ResponseHeaderTimeout: 5 * time.Minute},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				http.Error(w, "open-conduit gateway: "+err.Error(), http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	default:
		http.Error(w, "open-conduit gateway: send CONNECT or an absolute http:// URL", http.StatusBadRequest)
	}
}

func (g *Gateway) tunnel(w http.ResponseWriter, r *http.Request, addrs []string) {
	// Not the request's context: the server cancels it when a client that
	// has sent all it will half-closes, which a tunnel must outlast.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	up, err := g.dial(ctx, "tcp", addrs)
	if err != nil {
		http.Error(w, "open-conduit gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "open-conduit gateway: can't tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		up.Close()
		down.Close()
		return
	}
	pipe(down, buf.Reader, up)
}

// pipe copies both ways until either side closes; in holds what the
// client sent that was already read.
func pipe(down net.Conn, in *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		var src io.Reader = down
		if in != nil {
			src = in
		}
		_, _ = io.Copy(up, src)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(down, up)
		closeWrite(down)
		done <- struct{}{}
	}()
	<-done
	<-done
	up.Close()
	down.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func (g *Gateway) serveForward(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go func() {
			up, err := g.dialToolbelt()
			if err != nil {
				fmt.Fprintf(g.o.Log, "gateway: toolbelt forward: %v\n", err)
				c.Close()
				return
			}
			pipe(c, nil, up)
		}()
	}
}

// dialToolbelt reaches the execution's toolbelt on the host.
func (g *Gateway) dialToolbelt() (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	if path, ok := strings.CutPrefix(g.o.Forward, "unix:"); ok {
		return d.Dial("unix", path)
	}
	file, _ := strings.CutPrefix(g.o.Forward, "tcp:")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("no toolbelt port: %v", err)
	}
	port := strings.TrimSpace(string(raw))
	return d.Dial("tcp", net.JoinHostPort(g.o.ForwardHost, port))
}

// Parse reads `serve` flags: --listen, --allow (comma-separated, may
// repeat), --socket, --forward and --forward-host.
func Parse(args []string) (Options, error) {
	var o Options
	for i := 0; i < len(args); i++ {
		name := args[i]
		if i+1 >= len(args) {
			return Options{}, fmt.Errorf("%s needs a value", name)
		}
		value := args[i+1]
		i++
		switch name {
		case "--listen":
			o.Listen = value
		case "--allow":
			for _, h := range strings.Split(value, ",") {
				if h = strings.TrimSpace(h); h != "" {
					o.Allow = append(o.Allow, h)
				}
			}
		case "--socket":
			o.Socket = value
		case "--forward":
			o.Forward = value
		case "--forward-host":
			o.ForwardHost = value
		default:
			return Options{}, fmt.Errorf("unknown flag %s", name)
		}
	}
	return o, nil
}
