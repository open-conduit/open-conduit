package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/management"
)

// networkCerts writes a CA and two certificates it signs into dir: one for
// a server on 127.0.0.1 (server.pem, server.key) and one for a client
// (client.pem, client.key), with the CA as ca.pem. other.pem is a second,
// unrelated CA.
func networkCerts(t *testing.T, dir string) {
	t.Helper()
	write := func(name string, block *pem.Block) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newCA := func(name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour),
			NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(der)
		return cert, key, der
	}
	ca, caKey, caDER := newCA("open-conduit test CA")
	write("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	_, _, otherDER := newCA("another CA")
	write("other.pem", &pem.Block{Type: "CERTIFICATE", Bytes: otherDER})
	for i, leaf := range []struct {
		name  string
		usage x509.ExtKeyUsage
		ips   []net.IP
	}{{"server", x509.ExtKeyUsageServerAuth, []net.IP{net.ParseIP("127.0.0.1")}}, {"client", x509.ExtKeyUsageClientAuth, nil}} {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: leaf.name}, IPAddresses: leaf.ips,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{leaf.usage}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, _ := x509.MarshalECPrivateKey(key)
		write(leaf.name+".pem", &pem.Block{Type: "CERTIFICATE", Bytes: der})
		write(leaf.name+".key", &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
}

// serveNetwork serves the harness's management API on a management
// listener cfg describes (its Addr replaced by a free loopback port) and
// returns the https endpoint.
func serveNetwork(t *testing.T, h *managementHarness, cfg management.NetworkConfig) string {
	t.Helper()
	cfg.Addr = "127.0.0.1:0"
	ln, err := management.ListenNetwork(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := management.NetworkServer(management.NewNetwork(cfg, h.clock.Now).Handler(h.server))
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String()
}

func ownerToken(t *testing.T, h *managementHarness) string {
	t.Helper()
	f, err := client.ReadTokenFile(filepath.Join(h.state, "private", "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	return f.Token
}

func apiCode(err error) string {
	var api *client.Error
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

// The management listener serves the socket's API over mutual TLS to a
// client that pins the CA and presents its certificate, and refuses a
// client that does neither; the CLI reaches it the same way.
func TestManagementNetworkListenerMutualTLS(t *testing.T) {
	h := newManagementHarness(t)
	dir := t.TempDir()
	networkCerts(t, dir)
	at := func(name string) string { return filepath.Join(dir, name) }
	endpoint := serveNetwork(t, h, management.NetworkConfig{CertFile: at("server.pem"), KeyFile: at("server.key"), ClientCAFile: at("ca.pem")})
	token := ownerToken(t, h)
	ctx := context.Background()

	pinned := client.Options{CAFile: at("ca.pem"), ClientCertFile: at("client.pem"), ClientKeyFile: at("client.key")}
	c, err := client.NewWithOptions(endpoint, token, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Health(ctx); err != nil {
		t.Fatalf("health over the management listener: %v", err)
	}
	if _, err = c.Scopes(ctx, nil); err != nil {
		t.Fatalf("scopes over the management listener: %v", err)
	}

	for name, o := range map[string]client.Options{
		"no client certificate": {CAFile: at("ca.pem")},
		"another CA pinned":     {CAFile: at("other.pem"), ClientCertFile: at("client.pem"), ClientKeyFile: at("client.key")},
		"system roots":          {ClientCertFile: at("client.pem"), ClientKeyFile: at("client.key")},
	} {
		bad, err := client.NewWithOptions(endpoint, token, o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = bad.Health(ctx); err == nil {
			t.Fatalf("%s: the listener served the client", name)
		}
	}

	// A bearer is never sent in the clear, and TLS options are refused for
	// a Unix socket.
	if _, err = client.New(strings.Replace(endpoint, "https://", "http://", 1), token); err == nil {
		t.Fatal("an http:// endpoint was accepted")
	}
	if _, err = client.NewWithOptions(h.endpoint, token, client.Options{CAFile: at("ca.pem")}); err == nil {
		t.Fatal("TLS options were accepted for a Unix socket")
	}

	// The CLI, on another machine: an https endpoint, the operator's
	// token file, the pinned CA and the client certificate.
	tokenFile := filepath.Join(dir, "operator.token")
	b, _ := os.ReadFile(filepath.Join(h.state, "private", "operator.token"))
	if err = os.WriteFile(tokenFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": dir, "OPEN_CONDUIT_DATA_DIR": filepath.Join(dir, "elsewhere"), "OPEN_CONDUIT_ENDPOINT": endpoint, "OPEN_CONDUIT_TOKEN_FILE": tokenFile,
		"OPEN_CONDUIT_CA_FILE": at("ca.pem"), "OPEN_CONDUIT_CLIENT_CERT": at("client.pem"), "OPEN_CONDUIT_CLIENT_KEY": at("client.key")}
	var out, errOut bytes.Buffer
	if code := cli.RunOperator("test", "abc", []string{"health"}, cli.Env{Getenv: func(k string) string { return env[k] }, Cwd: dir}, &out, &errOut); code != 0 {
		t.Fatalf("CLI health over the management listener: exit %d: %s", code, errOut.String())
	}
}

// Over the network every call still needs an operator credential; the
// toolbelt is not served, attach is refused with the command to run on the
// daemon's machine, and a client that keeps failing authentication is
// refused until its window ends, even with a good credential.
func TestManagementNetworkListenerRefusals(t *testing.T) {
	h := newManagementHarness(t)
	dir := t.TempDir()
	networkCerts(t, dir)
	at := func(name string) string { return filepath.Join(dir, name) }
	endpoint := serveNetwork(t, h, management.NetworkConfig{CertFile: at("server.pem"), KeyFile: at("server.key")})
	token := ownerToken(t, h)
	ctx := context.Background()
	pinned := client.Options{CAFile: at("ca.pem")}
	c, err := client.NewWithOptions(endpoint, token, pinned)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachWork(ctx, "work/one", "attach-1"); apiCode(err) != "local_only" || !strings.Contains(err.Error(), "open-conduit work attach work/one") {
		t.Fatalf("attach over the network = %v, want local_only naming the command", err)
	}
	if _, err = c.ContainerAttach(ctx, "work/one", "attach-2"); apiCode(err) != "local_only" {
		t.Fatalf("container attach over the network = %v, want local_only", err)
	}
	// On the socket, the same call reaches the handler.
	if _, err = h.operatorClient().AttachWork(ctx, "work/one", "attach-3"); apiCode(err) == "local_only" {
		t.Fatal("attach refused on the owner-only socket")
	}
	// The toolbelt stays on per-execution sockets.
	if _, err = c.ToolbeltGuidance(ctx); apiCode(err) != "not_found" {
		t.Fatalf("toolbelt over the network = %v, want not_found", err)
	}

	wrong, err := client.NewWithOptions(endpoint, "not-a-credential", pinned)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < management.AuthFailureLimit; i++ {
		if _, err = wrong.Scopes(ctx, nil); apiCode(err) != "unauthenticated" {
			t.Fatalf("failure %d = %v, want unauthenticated", i, err)
		}
	}
	if _, err = wrong.Scopes(ctx, nil); apiCode(err) != "too_many_auth_failures" {
		t.Fatalf("after the limit = %v, want too_many_auth_failures", err)
	}
	if _, err = c.Scopes(ctx, nil); apiCode(err) != "too_many_auth_failures" {
		t.Fatalf("a good credential from a refused client = %v, want too_many_auth_failures", err)
	}
	// The socket is never limited.
	if _, err = h.operatorClient().Scopes(ctx, nil); err != nil {
		t.Fatalf("socket refused while the network client is limited: %v", err)
	}
	h.clock.Advance(management.AuthFailureWindow)
	if _, err = c.Scopes(ctx, nil); err != nil {
		t.Fatalf("still refused after the window: %v", err)
	}
}

// Behind a trusted local proxy the listener serves plaintext on loopback,
// and the proxy's X-Forwarded-For names the client for its limits.
func TestManagementNetworkListenerTrustedProxy(t *testing.T) {
	h := newManagementHarness(t)
	cfg := management.NetworkConfig{Addr: "127.0.0.1:0", TrustedProxy: true}
	ln, err := management.ListenNetwork(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := management.NetworkServer(management.NewNetwork(cfg, h.clock.Now).Handler(h.server))
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	get := func(from, token string) int {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/v1/scopes", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-For", from)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < management.AuthFailureLimit; i++ {
		get("203.0.113.7", "wrong")
	}
	if code := get("203.0.113.7", ownerToken(t, h)); code != http.StatusTooManyRequests {
		t.Fatalf("the limited client got %d", code)
	}
	if code := get("198.51.100.1, 203.0.113.9", ownerToken(t, h)); code != http.StatusOK {
		t.Fatalf("another client behind the proxy got %d", code)
	}
}
