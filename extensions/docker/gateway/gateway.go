// Package gateway is the program in the Docker provider's gateway image.
// One gateway container runs beside each launch's container, on the
// execution's internal network (as "gateway") and on an egress network of
// its own, and does two things:
//
//   - an allowlisting HTTP proxy: CONNECT and absolute-form HTTP requests
//     reach only the hosts the grant's network.allow lists, and anything
//     else gets 403;
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
	// Dial reaches an allowed host (net.Dialer's when nil).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
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

func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		host := withPort(r.Host, "443")
		if !g.allowed(host) {
			g.refuse(w, http.StatusForbidden, "CONNECT", host)
			return
		}
		g.tunnel(w, r, host)
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		host := withPort(r.URL.Host, "80")
		if !g.allowed(host) {
			g.refuse(w, http.StatusForbidden, "GET", host)
			return
		}
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Host = host
				pr.Out.Host = r.URL.Host
			},
			Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				// Dial the host checked, whatever the transport resolved.
				return g.o.Dial(ctx, network, host)
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

func (g *Gateway) tunnel(w http.ResponseWriter, r *http.Request, host string) {
	// Not the request's context: the server cancels it when a client that
	// has sent all it will half-closes, which a tunnel must outlast.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	up, err := g.o.Dial(ctx, "tcp", host)
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
