package integration_test

// git's credential in a container against real Docker (make
// container-test and the container CI job): a GitHub App connection mints
// a token for the Work's one repository through the toolbelt, and it stops
// working once the execution ends. GitHub is a stand-in (fakegithub) on the
// gateway's egress network, reached over TLS through the gateway as
// github.com; the test calls no real service.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/docker"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/storage"
)

// dockerGitImage is the image the git test's agent and stand-in GitHub
// run, pinned by digest: Alpine with git 2.47.
const dockerGitImage = "alpine/git@sha256:062a01ad7a0eb17cff382bc5e26086b4d710e56dfdfdf001109a49b6d9bd378c"

// gitClaude stands in for claude: it takes its turn's message and runs git
// as an agent would, writing "name: ALLOWED|DENIED" for each attempt to
// $FAKE_DIR/probes. The first execution pushes to the Work's repository
// with nothing but git's configured helper, keeps the token the helper is
// given and its toolbelt token, and tries that token on a second
// repository. The next one, once the first has ended, tries both of the
// first's tokens again, and pushes with its own.
const gitClaude = `#!/bin/sh
session=
while [ $# -gt 0 ]; do
  case "$1" in --session-id|--resume) session=$2; shift ;; esac
  shift
done
f=${FAKE_DIR:?}
read -r line
export GIT_TERMINAL_PROMPT=0
t() { n=$1; shift; if "$@" >>"$f/git.log" 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi >> "$f/probes"; }
cred() { printf 'protocol=https\nhost=github.com\n\n' | os git-credential get 2>>"$f/git.log" | sed -n 's/^password=//p'; }
if [ ! -f "$f/minted" ]; then
  t push_own git push https://github.com/octo/widgets.git HEAD:refs/heads/agent
  tok=$(cred); printf %s "$tok" > "$f/minted"; cp "$OS_TOKEN_FILE" "$f/belt"
  t minted test -n "$tok"
  t push_other_with_token git push "https://x-access-token:$tok@github.com/octo/other.git" HEAD:refs/heads/agent
  t read_other git ls-remote https://github.com/octo/other.git
else
  tok=$(cat "$f/minted")
  t ended_token_push git push "https://x-access-token:$tok@github.com/octo/widgets.git" HEAD:refs/heads/ended
  t ended_helper sh -c 'printf "protocol=https\nhost=github.com\n\n" | OS_TOKEN_FILE="$1" os git-credential get | grep -q "^password="' - "$f/belt"
  t push_next git push https://github.com/octo/widgets.git HEAD:refs/heads/next
fi
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s","num_turns":1,"total_cost_usd":0.01,"structured_output":{"state":"review","summary":"ok","handoff":"note"}}\n' "$session"
`

var gitExpect = map[string]string{
	"push_own": "ALLOWED", "minted": "ALLOWED", "push_other_with_token": "DENIED", "read_other": "DENIED",
	"ended_token_push": "DENIED", "ended_helper": "DENIED", "push_next": "ALLOWED",
}

// gitStore serves the one Work the GitHub tools bind a call to.
type gitStore struct{ w domain.Work }

type gitTx struct {
	storage.Tx
	w domain.Work
}

func (s gitStore) Transact(_ context.Context, _ storage.Guard, f func(storage.Tx) error) error {
	return f(gitTx{w: s.w})
}

func (t gitTx) Work(id domain.ID) (domain.Work, error) {
	if id != t.w.ID {
		return domain.Work{}, storage.ErrNotFound
	}
	return t.w, nil
}

// testCerts writes a CA and a certificate it signs for github.com and
// api.github.com into dir: ca.pem, cert.pem and key.pem.
func testCerts(t *testing.T, dir string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "open-conduit test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "github.com"}, DNSNames: []string{"github.com", "api.github.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	for name, block := range map[string]*pem.Block{"ca.pem": {Type: "CERTIFICATE", Bytes: caDER}, "cert.pem": {Type: "CERTIFICATE", Bytes: der}, "key.pem": {Type: "EC PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// buildFakeGitHub builds fakegithub for the context's architecture.
func buildFakeGitHub(t *testing.T, l *dockerLab, dir string) {
	t.Helper()
	arch, err := l.docker("version", "--format", "{{.Server.Arch}}")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "fakegithub"), "github.com/open-conduit/open-conduit/integration/fakegithub")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fakegithub: %v\n%s", err, out)
	}
}

// TestDockerGitCredential: claude-code in a container, signed in to
// GitHub through an App, pushes to the Work's repository with plain git:
// git's helper is the toolbelt's os, which gets a token the host minted
// for that one repository. The token can't push to, or read, a second
// repository the App is installed on. Once the execution has ended (its
// toolbelt token revoked, and the host's revocation of what was minted for
// it, as the segment's Settle does), neither its token nor its helper
// works, and the next execution pushes with a token of its own.
func TestDockerGitCredential(t *testing.T) {
	l := newDockerLab(t)
	ctx := context.Background()
	if out, err := l.docker("pull", "-q", dockerGitImage); err != nil {
		t.Fatalf("pulling %s: %v %s", dockerGitImage, err, out)
	}
	bin, fake, certs, fg := filepath.Join(l.root, "bin"), filepath.Join(l.root, "fake"), filepath.Join(l.root, "certs"), filepath.Join(l.root, "fg")
	execState := filepath.Join(l.state, "executors", "claude-code")
	home := filepath.Join(l.state, "homes", "work")
	for _, d := range []string{bin, fake, certs, fg, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(gitClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	testCerts(t, certs)
	ca, _ := os.ReadFile(filepath.Join(certs, "ca.pem"))
	if err := os.WriteFile(filepath.Join(bin, "ca.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	appKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, _ := x509.MarshalPKIXPublicKey(&appKey.PublicKey)
	if err := os.WriteFile(filepath.Join(certs, "app.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0o644); err != nil {
		t.Fatal(err)
	}
	buildFakeGitHub(t, l, fg)
	// The workspace has a commit to push.
	commit := exec.Command("git", "-C", l.ws, "-c", "user.name=agent", "-c", "user.email=agent@example.com", "commit", "-q", "--allow-empty", "-m", "work")
	commit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	head, _ := exec.Command("git", "-C", l.ws, "rev-parse", "HEAD").Output()

	// GitHub, as github.com on the first execution's egress network.
	egress := l.egress(t, "git-1", 181)
	id, err := l.docker("run", "-d", "--rm", "--label", extension.ContainerLabelHost+"="+l.host, "--network", egress, "--network-alias", "github.com",
		"-p", "127.0.0.1::443", "--read-only", "--tmpfs", "/tmp", "-v", fg+":/fg:ro", "-v", certs+":/certs:ro", "--entrypoint", "/fg/fakegithub", dockerGitImage,
		"-repo", "octo/widgets", "-extra", "octo/other", "-app", "42", "-app-key", "/certs/app.pem", "-cert", "/certs/cert.pem", "-cert-key", "/certs/key.pem")
	if err != nil {
		t.Fatalf("the stand-in GitHub: %v %s", err, id)
	}
	published, err := l.docker("port", id, "443/tcp")
	if err != nil {
		t.Fatalf("its port: %v %s", err, published)
	}
	published = strings.Fields(published)[0]
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, published)
		}}}
	ready := false
	for i := 0; i < 100 && !ready; i++ {
		if res, err := httpClient.Get("https://api.github.com/"); err == nil {
			res.Body.Close()
			ready = true
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !ready {
		logs, _ := l.docker("logs", id)
		t.Fatalf("the stand-in GitHub never answered:\n%s", logs)
	}

	// The host's GitHub connection, an App, and the tools over it.
	conn, _ := gh.ParseConnection([]byte(`{"auth":"app","app_id":"42"}`))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)}))
	client, err := gh.NewClient(conn, func(context.Context, string) (string, error) { return keyPEM, nil }, gh.ClientOptions{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	const ns = "github/github.com/octo/widgets"
	settings, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"scope","repo":"octo/widgets"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	work := domain.Work{ID: "work", ScopeID: "scope", CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: gh.IssueKey(3, "")}}}
	tools, err := gh.NewTools(gh.ToolsOptions{Store: gitStore{work}, Sources: func() []gh.Source { return []gh.Source{{Settings: settings, Client: client}} }})
	if err != nil {
		t.Fatal(err)
	}
	var credOp extension.Operation
	for _, op := range tools.Registration().Operations {
		if op.Name == gh.OpGitCredential {
			credOp = op
		}
	}

	// Each execution's toolbelt, as the daemon serves it: the execution's
	// own token, until its end revokes it, reaches github.git.credential.
	var mu sync.Mutex
	valid := map[string]domain.ID{}
	endpoints, err := management.NewEndpoints(nil, l.state, l.root+"/s", func(exec domain.ID) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var call struct {
				Operation string
				Input     json.RawMessage
			}
			_ = json.NewDecoder(r.Body).Decode(&call)
			mu.Lock()
			owner, ok := valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !ok || owner != exec || call.Operation != gh.OpGitCredential {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"execution authority rejected"}}`)
				return
			}
			out, err := credOp.Handler(r.Context(), extension.Authority{ScopeID: "scope", WorkID: "work", ExecutionID: exec}, domain.Payload{Data: call.Input})
			var refused *extension.OperationError
			if errors.As(err, &refused) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": refused.Code, "message": refused.Message}})
				return
			} else if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"error":{"code":"unavailable","message":"failed"}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"output": map[string]any{"schema": out.Schema, "data": out.Data}}})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { endpoints.Shutdown(context.Background()) })

	e, err := claudecode.New(claudecode.Options{StateDir: execState, Store: reference.New(), ToolbeltDir: bin,
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} }, Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	grant := `{"image":"` + dockerGitImage + `","write":["{workspace}","` + fake + `"],"read":["` + bin + `"],"network":{"allow":["api.anthropic.com","github.com"]},"provider":{"context":"` + l.dctx + `"}}`
	cfg, _ := json.Marshal(map[string]any{"binary": filepath.Join(bin, "claude"), "playbook": "-", "env": map[string]string{"FAKE_DIR": fake, "GIT_SSL_CAINFO": filepath.Join(bin, "ca.pem")}})
	turn := func(exec domain.ID) extension.Result {
		t.Helper()
		token := "belt-" + string(exec)
		endpoint, tokenFile, err := endpoints.Serve(ctx, exec, token)
		if err != nil {
			t.Fatal(err)
		}
		socket := strings.TrimPrefix(endpoint, "unix://")
		if l.provider.Forward() == "tcp" {
			host, err := l.provider.ForwardListen(ctx, l.dctx)
			if err == nil {
				_, err = endpoints.ServeTCP(exec, host)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		mu.Lock()
		valid[token] = exec
		mu.Unlock()
		b := extension.BindContainer(extension.ContainerBinding{Provider: l.provider, Owner: extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: exec},
			Config: domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(grant)}, Host: l.host, Segment: 1,
			Protected: []string{l.state, l.config}, Shared: []string{filepath.Dir(socket), filepath.Dir(tokenFile)}, Toolbelt: socket,
			RunRoot: filepath.Join(execState, "runs"), Records: filepath.Join(l.state, "runs", string(exec), "container"), Home: l.home, WorkHome: home,
			Pin: func(c extension.ContainerPin) error {
				l.mu.Lock()
				l.pins = append(l.pins, c)
				l.mu.Unlock()
				return nil
			}})
		r := extension.RunRequest{Work: work, Execution: domain.Execution{ID: exec, WorkID: work.ID, Attempt: 1, Lease: domain.Lease{Epoch: 1}},
			Trigger: domain.Event{ID: domain.ID("ev-" + exec), Type: "note"},
			Context: extension.ExecutionContext{Config: domain.Payload{Data: cfg}, Container: b, ContainerHome: home, Resources: b.Resources(dockerWorkspace{l.ws}),
				ToolbeltEndpoint: endpoint, ToolbeltToken: token, ToolbeltTokenFile: tokenFile},
			PersistRecovery: func(domain.Payload) error { return nil }}
		h, err := e.Run(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		res := b.Settle(waitResult(t, h))
		// The execution ends: the host revokes its toolbelt token and the
		// git tokens minted for it.
		mu.Lock()
		delete(valid, token)
		mu.Unlock()
		if err := tools.RevokeGitTokens(ctx, exec); err != nil {
			t.Fatalf("revoking %s's git tokens: %v", exec, err)
		}
		return res
	}
	report := func() string {
		raw, _ := os.ReadFile(filepath.Join(fake, "git.log"))
		probes, _ := os.ReadFile(filepath.Join(fake, "probes"))
		logs, _ := l.docker("logs", id)
		return fmt.Sprintf("probes:\n%s\ngit:\n%s\nGitHub:\n%s", probes, raw, logs)
	}
	if res := turn("git-1"); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("the first execution failed: %q\n%s", res.Outcome.Summary, report())
	}
	l.egress(t, "git-2", 182)
	if out, err := l.docker("network", "connect", "--alias", "github.com", docker.ExecutionNetwork(l.host, "git-2")+"-egress", id); err != nil {
		t.Fatalf("connecting the stand-in GitHub: %v %s", err, out)
	}
	if res := turn("git-2"); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("the second execution failed: %q\n%s", res.Outcome.Summary, report())
	}
	raw, _ := os.ReadFile(filepath.Join(fake, "probes"))
	got := probeResults(string(raw))
	for k, want := range gitExpect {
		if got[k] != want {
			t.Errorf("%s: %q from inside the container, want %s", k, got[k], want)
		}
	}
	// What reached GitHub: the pushes with the helper, and nothing else.
	ref := func(repo, name string) string {
		out, _ := l.docker("exec", id, "git", "--git-dir", "/tmp/git/"+repo+".git", "rev-parse", "--verify", "-q", "refs/heads/"+name)
		return out
	}
	want := strings.TrimSpace(string(head))
	for _, c := range []struct{ repo, ref, want string }{
		{"octo/widgets", "agent", want}, {"octo/widgets", "next", want}, {"octo/widgets", "ended", ""}, {"octo/other", "agent", ""},
	} {
		if got := ref(c.repo, c.ref); got != c.want {
			t.Errorf("%s %s is %q on GitHub, want %q", c.repo, c.ref, got, c.want)
		}
	}
	if t.Failed() || os.Getenv("LAB_GIT_REPORT") != "" {
		t.Log(report())
	}
}
