package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/gateway"
)

// behindGateway is a fake remote whose binding allows two names, with a
// direct connection out refused.
func behindGateway(t *testing.T) *fakeRemote {
	t.Helper()
	if _, err := exec.LookPath("nc"); err != nil {
		t.Fatal("the egress check needs nc")
	}
	f := newFakeRemote(t)
	f.settings.Network = NetworkGateway
	raw := strings.Replace(string(f.configFor(f.settings).Data), `"write":`, `"network":{"allow":["allowed.test","private.test"]},"write":`, 1)
	f.config = domain.Payload{Schema: extension.ContainerConfigSchema, Data: []byte(raw)}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.p.o.EgressProbe = []string{l.Addr().String()}
	l.Close()
	return f
}

// TestLaunchThroughGateway: a run on a remote reaches an allowed host
// through the gateway, whatever proxy its launch named, and is refused a
// host off the list and a listed name that resolves to a private address.
func TestLaunchThroughGateway(t *testing.T) {
	f := behindGateway(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "upstream") }))
	defer upstream.Close()
	script := `for h in allowed.test denied.test private.test; do echo "$h $(curl -s -m 10 -o /dev/null -w '%{http_code}' "http://$h/")"; done > codes.txt`
	launch := f.launch(script)
	launch.Env = append(launch.Env, "http_proxy=http://127.0.0.1:1")
	plan, err := f.bind(nil).Launch(context.Background(), f.needs(), launch)
	if err != nil {
		t.Fatal(err)
	}
	if want := extension.EgressEnforced.Describe("the remote's only way out is the gateway, over its ssh connection"); !strings.Contains(f.pins[0].Summary, want) {
		t.Errorf("the plan's summary %q doesn't say %q", f.pins[0].Summary, want)
	}
	var out, errs bytes.Buffer
	code := RunHelper(plan.Argv[3:], HelperOptions{Stdin: bytes.NewReader(nil), Stdout: &out, Stderr: &errs, Interval: time.Millisecond,
		Gateway: gateway.Options{
			Resolve: func(_ context.Context, host string) ([]net.IP, error) {
				if host == "private.test" {
					return []net.IP{net.ParseIP("10.0.0.5")}, nil
				}
				return []net.IP{net.ParseIP("203.0.113.5")}, nil
			},
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			},
		}})
	if code != 0 {
		t.Fatalf("the helper exited %d: %s%s", code, out.String(), errs.String())
	}
	want := "allowed.test 200\ndenied.test 403\nprivate.test 403\n"
	if got := read(t, filepath.Join(f.ws, "codes.txt")); got != want {
		t.Errorf("codes:\n%swant:\n%s", got, want)
	}
}

// TestGatewayRefusesAnOpenRemote: a remote that connects out directly
// isn't behind its gateway, so nothing is created on it.
func TestGatewayRefusesAnOpenRemote(t *testing.T) {
	f := behindGateway(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f.p.o.EgressProbe = []string{l.Addr().String()}
	_, err = f.bind(nil).Launch(context.Background(), f.needs(), f.launch("true"))
	if !errors.Is(err, extension.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "only way out") {
		t.Fatalf("launch: %v, want it refused for its way out", err)
	}
	if len(f.pins) != 0 {
		t.Fatalf("pins %+v: a run was created", f.pins)
	}
}

// TestStubNeedsItsGateway: a stub for a run behind a gateway, started
// without one, runs nothing and removes its credentials.
func TestStubNeedsItsGateway(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	ran := filepath.Join(dir, "ran")
	start := filepath.Join(dir, "start")
	write(t, start, stub("", nil, dir, filepath.Join(dir, "term"), []string{"touch", ran}, true), 0o700)
	write(t, filepath.Join(dir, SecretsFile), "export S=1\n", 0o600)
	err := exec.Command("/bin/sh", start, "marker", "").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 125 {
		t.Fatalf("the stub: %v, want exit 125", err)
	}
	for _, gone := range []string{ran, filepath.Join(dir, SecretsFile)} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s exists", gone)
		}
	}
}
