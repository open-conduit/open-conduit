package management

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// NetworkConfig is the opt-in management listener on a TCP address: the
// same API the owner-only socket serves, for clients on other machines. It
// is off unless Addr is set, and it never serves plaintext to the network:
// it needs a certificate and key (TLS), and ClientCA turns on mutual TLS.
// The one exception is TrustedProxy on a loopback address, for a proxy on
// the same machine that terminates TLS itself.
type NetworkConfig struct {
	// Addr is the TCP address to listen on, for example 0.0.0.0:8443.
	Addr string
	// CertFile and KeyFile are the listener's PEM certificate (chain) and
	// private key. They are read again when either file changes, so a
	// renewed certificate needs no restart.
	CertFile, KeyFile string
	// ClientCAFile, when set, makes every connection present a client
	// certificate that this PEM bundle verifies. It is a second factor:
	// each request still needs an operator credential.
	ClientCAFile string
	// TrustedProxy serves plaintext on a loopback Addr to a proxy on the
	// same machine that terminates TLS. The proxy's X-Forwarded-For then
	// names the client for authentication-failure limits.
	TrustedProxy bool
}

// Enabled reports whether any of the listener's settings is given.
func (c NetworkConfig) Enabled() bool {
	return c.Addr != "" || c.CertFile != "" || c.KeyFile != "" || c.ClientCAFile != "" || c.TrustedProxy
}

// Check refuses a listener that is half-configured or would serve
// plaintext to the network. It reads no files.
func (c NetworkConfig) Check() error {
	if !c.Enabled() {
		return nil
	}
	if c.Addr == "" {
		return errors.New("management listener: TLS or proxy settings need --management-addr")
	}
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("management listener: --management-addr %q is not host:port", c.Addr)
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("management listener: give both --management-tls-cert and --management-tls-key")
	}
	tlsOn := c.CertFile != ""
	switch {
	case c.TrustedProxy && tlsOn:
		return errors.New("management listener: --management-trusted-proxy serves plaintext to a local proxy that terminates TLS; give it or a certificate, not both")
	case c.TrustedProxy && !loopbackHost(host):
		return fmt.Errorf("management listener: --management-trusted-proxy needs a loopback --management-addr (127.0.0.1 or ::1), not %q: plaintext never leaves the machine", host)
	case c.ClientCAFile != "" && !tlsOn:
		return errors.New("management listener: --management-client-ca needs --management-tls-cert and --management-tls-key")
	case !tlsOn && !c.TrustedProxy:
		return errors.New("management listener: --management-addr needs --management-tls-cert and --management-tls-key (or, on a loopback address behind a local TLS-terminating proxy, --management-trusted-proxy); it never serves plaintext")
	}
	return nil
}

// loopbackHost reports whether host is a loopback IP literal. A name is
// not trusted to stay loopback.
func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ListenNetwork binds the management listener c describes, after Check.
// The certificate, key and client CA are loaded now, so a bad file is a
// startup error, never a fallback to plaintext.
func ListenNetwork(c NetworkConfig) (net.Listener, error) {
	if !c.Enabled() {
		return nil, errors.New("management listener: not configured")
	}
	if err := c.Check(); err != nil {
		return nil, err
	}
	var cfg *tls.Config
	if c.CertFile != "" {
		certs := &certReloader{cert: c.CertFile, key: c.KeyFile}
		if err := certs.load(); err != nil {
			return nil, err
		}
		cfg = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get, NextProtos: []string{"h2", "http/1.1"}}
		if c.ClientCAFile != "" {
			b, err := os.ReadFile(c.ClientCAFile)
			if err != nil {
				return nil, fmt.Errorf("management listener: client CA unreadable: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(b) {
				return nil, errors.New("management listener: client CA holds no PEM certificate")
			}
			cfg.ClientCAs, cfg.ClientAuth = pool, tls.RequireAndVerifyClientCert
		}
	}
	ln, err := net.Listen("tcp", c.Addr)
	if err != nil {
		return nil, fmt.Errorf("management listener: %w", err)
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	return ln, nil
}

// certReloader serves the listener's certificate, loading it again when
// the certificate or key file changes, and keeping the last good one while
// a renewal is half-written.
type certReloader struct {
	cert, key string
	mu        sync.Mutex
	current   *tls.Certificate
	stamp     string
}

func (r *certReloader) fileStamp() string {
	var b strings.Builder
	for _, p := range []string{r.cert, r.key} {
		if info, err := os.Stat(p); err == nil {
			b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10) + "/" + strconv.FormatInt(info.Size(), 10) + ";")
		}
	}
	return b.String()
}

func (r *certReloader) load() error {
	stamp := r.fileStamp()
	pair, err := tls.LoadX509KeyPair(r.cert, r.key)
	if err != nil {
		return fmt.Errorf("management listener: certificate and key unusable: %w", err)
	}
	r.mu.Lock()
	r.current, r.stamp = &pair, stamp
	r.mu.Unlock()
	return nil
}

func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	changed := r.fileStamp() != r.stamp
	current := r.current
	r.mu.Unlock()
	if changed {
		if r.load() == nil {
			r.mu.Lock()
			current = r.current
			r.mu.Unlock()
		}
	}
	return current, nil
}

// NetworkServer is the http.Server for the management listener: header,
// read, write and idle timeouts the owner-only socket never needed, and a
// header cap with room for an identity provider's bearer.
func NetworkServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
}

// Network marks the requests of the management listener: they come from
// another machine, so the server refuses what only makes sense on the
// daemon's own (attach), refuses execution tokens, and limits failed
// authentication by client. One Network outlives configuration reloads, so
// its limits do too.
type Network struct {
	trustedProxy bool
	limiter      *failureLimiter
	// OnThrottle, when set, is told when a client is refused for too many
	// failed authentications, once each time its limit is reached.
	OnThrottle func(client string)
}

// Authentication-failure limits: once a client has failed authentication
// AuthFailureLimit times within AuthFailureWindow, its further failures are
// refused (429) until the window ends. At most maxTrackedClients are tracked.
const (
	AuthFailureLimit  = 10
	AuthFailureWindow = time.Minute
	maxTrackedClients = 4096
)

// NewNetwork makes the marker for c's listener, timing its limits by now.
func NewNetwork(c NetworkConfig, now func() time.Time) *Network {
	return &Network{trustedProxy: c.TrustedProxy, limiter: &failureLimiter{now: now, seen: map[string]*failures{}}}
}

type networkKey struct{}

// remoteCall is a management listener request's client.
type remoteCall struct {
	network *Network
	client  string
}

// Handler marks each request as the management listener's, with its
// client, and serves it with next (the current server).
func (n *Network) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := &remoteCall{network: n, client: n.clientOf(r)}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), networkKey{}, rc)))
	})
}

// clientOf names a request's client: its address, or behind a trusted
// proxy the last address the proxy appended to X-Forwarded-For.
func (n *Network) clientOf(r *http.Request) string {
	if n.trustedProxy {
		if fwd := r.Header.Values("X-Forwarded-For"); len(fwd) > 0 {
			hops := strings.Split(fwd[len(fwd)-1], ",")
			if last := strings.TrimSpace(hops[len(hops)-1]); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// remote is the management listener request's client, or nil on the
// owner-only socket or an execution's.
func remote(ctx context.Context) *remoteCall {
	rc, _ := ctx.Value(networkKey{}).(*remoteCall)
	return rc
}

// localOnly reports whether a route only makes sense on the daemon's own
// machine: attach returns a command and directory for the caller to run
// there, a container attach a docker command for the daemon's Docker, and
// installing a release replaces the binary there.
func localOnly(rt *route) bool {
	if strings.Join(rt.pattern, "/") == "releases/install" {
		return true
	}
	if len(rt.pattern) != 3 || rt.pattern[0] != "work" {
		return false
	}
	return rt.pattern[2] == "attach" || rt.pattern[2] == "container-attach"
}

// errLocalOnly refuses a local-only route over the network, naming what
// to run on the daemon's machine instead.
func errLocalOnly(rt *route, params []string) error {
	if rt.pattern[0] == "releases" {
		return fail(409, "local_only", "this replaces the binary on the daemon's machine, so the management listener refuses it: run `open-conduit upgrade` there")
	}
	cmd := "open-conduit work attach " + params[0]
	if rt.pattern[2] == "container-attach" {
		cmd = "open-conduit container attach " + params[0]
	}
	return fail(409, "local_only", "this runs a command on the daemon's machine, so the management listener refuses it: run `"+cmd+"` there")
}

type failures struct {
	count  int
	since  time.Time
	warned bool
}

// failureLimiter counts each client's failed authentications in a fixed
// window. Only failures are counted and refused: a request whose
// credential authenticates is always served, so a client sharing an
// address with an attacker (behind a load balancer or a NAT) is never held
// out.
type failureLimiter struct {
	now  func() time.Time
	mu   sync.Mutex
	seen map[string]*failures
}

// overflowClient is the key every new client shares while the table is
// full of clients at their limit, so a flood of addresses can neither
// evict a blocked client nor get unlimited failures.
const overflowClient = "*"

// limiterKey is the client's key: its address, or for IPv6 its /64, which
// a single host can usually draw addresses from at will.
func limiterKey(client string) string {
	ip := net.ParseIP(client)
	if ip == nil || ip.To4() != nil {
		return client
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// fail counts one failed authentication by client. It returns how long
// the client is refused, 0 while it is under the limit, and whether this
// failure took it to the limit for the first time this window.
func (l *failureLimiter) fail(client string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := limiterKey(client)
	f := l.seen[key]
	if f == nil && len(l.seen) >= maxTrackedClients && !l.evict(now) {
		key = overflowClient
		f = l.seen[key]
	}
	if f == nil || !now.Before(f.since.Add(AuthFailureWindow)) {
		f = &failures{since: now}
		l.seen[key] = f
	}
	f.count++
	if f.count <= AuthFailureLimit {
		return 0, false
	}
	first := !f.warned
	f.warned = true
	return f.since.Add(AuthFailureWindow).Sub(now), first
}

// evict makes room for one client: it drops every ended window, and with
// none ended, the oldest window still under the limit. A client at its
// limit is never dropped; with every window at its limit, it reports false.
func (l *failureLimiter) evict(now time.Time) bool {
	oldest, at := "", time.Time{}
	freed := false
	for k, f := range l.seen {
		if !now.Before(f.since.Add(AuthFailureWindow)) {
			delete(l.seen, k)
			freed = true
			continue
		}
		if k != overflowClient && f.count <= AuthFailureLimit && (oldest == "" || f.since.Before(at)) {
			oldest, at = k, f.since
		}
	}
	if freed {
		return true
	}
	if oldest == "" {
		return false
	}
	delete(l.seen, oldest)
	return true
}

// CursorKey is the key list cursors are signed with, kept at path (an
// owner-only file, made on first use), so a cursor a client holds survives
// a restart of the daemon. A cursor still expires 15 minutes after it was issued.
func CursorKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		info, serr := os.Lstat(path)
		if serr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("cursor key file must be a private 0600 regular file")
		}
		if len(b) != cursorKeySize {
			return nil, errors.New("cursor key file is malformed")
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, cursorKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".cursor-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(key); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	return key, nil
}

// SharedCursorKey is the key list cursors are signed with on state several
// hosts reach, kept in the store and made on first use, so a cursor a client
// holds survives a takeover as CursorKey's survives a restart. Anything that can
// read the store can read it, which gives no more than that access already
// does: a cursor only resumes a list its caller is authorised for.
func SharedCursorKey(ctx context.Context, store storage.Store, now func() time.Time) ([]byte, error) {
	var key []byte
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		key = nil
		b, err := tx.Get(authNamespace, cursorKeyRecord)
		if err == nil {
			if err = json.Unmarshal(b, &key); err != nil || len(key) != cursorKeySize {
				return errors.New("stored cursor key is malformed")
			}
			return nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		key = make([]byte, cursorKeySize)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		b, err = json.Marshal(key)
		if err != nil {
			return err
		}
		return tx.Put(authNamespace, cursorKeyRecord, b)
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

const (
	cursorKeySize   = 32
	cursorKeyRecord = "cursor_key"
)
