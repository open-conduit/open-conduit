package integration_test

// The gateway against real Docker: the allowlisting proxy, a tool that
// ignores it, and the toolbelt forward, from inside a container; and the
// shell executor running a binding in a container end to end. Like the
// tests in container_test.go they run only under make container-test and
// the container CI job, whose second pass forces the tcp forward macOS uses
// (OPEN_CONDUIT_CONTAINER_FORWARD=tcp).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/docker"
	"github.com/open-conduit/open-conduit/internal/management"
)

// labToolbelt is an execution's toolbelt as the daemon serves it, with a
// stand-in handler: read_context answers {"via": "the forward"} to the
// execution's own token and anything else is refused. It returns the
// socket, the token file and the token.
func (l *dockerLab) labToolbelt(t *testing.T, id domain.ID) (endpoints *management.Endpoints, socket, tokenFile, token string) {
	t.Helper()
	token = "token-" + string(id)
	e, err := management.NewEndpoints(nil, l.state, l.root+"/s", func(domain.ID) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var call struct{ Operation string }
			_ = json.NewDecoder(r.Body).Decode(&call)
			if r.URL.Path != "/v1/toolbelt/call" || r.Header.Get("Authorization") != "Bearer "+token || call.Operation != "read_context" {
				http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":{"context":{"schema":"lab.context.v1","data":{"via":"the forward"}}}}`)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	endpoint, file, err := e.Serve(context.Background(), id, token)
	if err != nil {
		t.Fatal(err)
	}
	socket = strings.TrimPrefix(endpoint, "unix://")
	// The tcp forward reaches it on a port the host serves, as the
	// daemon's binding does.
	if l.provider.Forward() == docker.ForwardTCP {
		host, err := l.provider.ForwardListen(context.Background(), l.dctx)
		if err == nil {
			_, err = e.ServeTCP(id, host)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return e, socket, file, token
}

// toolbeltBinding is the lab's binding with the execution's toolbelt, as
// bootstrap binds it: the socket, and its directory shared read-only.
func (l *dockerLab) toolbeltBinding(config string, id domain.ID, socket string) *extension.BoundContainer {
	return extension.BindContainer(extension.ContainerBinding{
		Provider:  l.provider,
		Owner:     extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: id},
		Config:    domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(config)},
		Host:      l.host,
		Segment:   1,
		Protected: []string{l.state, l.config},
		Shared:    []string{l.run, filepath.Dir(socket)},
		RunRoot:   l.runRoot,
		Toolbelt:  socket,
		Records:   l.records,
		Home:      l.home,
		Pin: func(c extension.ContainerPin) error {
			l.mu.Lock()
			l.pins = append(l.pins, c)
			l.mu.Unlock()
			return nil
		},
	})
}

// gatewayProbes print "name: ALLOWED|DENIED" from inside the container.
const gatewayProbes = `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi; }
get() { i=0; while [ $i -lt 20 ]; do out=$(wget -q -T 3 -O - "$@" 2>&1) && [ "$out" = upstream ] && return 0; case "$out" in *403*) return 1;; esac; i=$((i+1)); sleep 0.5; done; return 1; }
connect() { printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nGET / HTTP/1.0\r\n\r\n' "$1" "$1" | nc -w 5 gateway 3128 > /tmp/connect 2>&1; grep -q " $2 " /tmp/connect && { [ "$2" != 200 ] || grep -q upstream /tmp/connect; }; }
t allowed_http get http://allowed.test:8080/
t denied_http sh -c 'out=$(wget -q -T 3 -O - http://denied.test:8080/ 2>&1); case "$out" in *403*) exit 0;; esac; exit 1'
t allowed_connect connect allowed.test:8080 200
t denied_connect connect denied.test:8080 403
t ignores_proxy_by_name wget -Y off -q -T 3 -O /dev/null http://allowed.test:8080/
t ignores_proxy_by_ip wget -Y off -q -T 3 -O /dev/null "http://$LAB_UPSTREAM:8080/"
t outside_dns nslookup allowed.test
t toolbelt sh -c 'os context | grep -q "the forward"'
t toolbelt_endpoint sh -c '[ "$OS_ENDPOINT" = unix:///run/open-conduit/toolbelt/toolbelt.sock ]'
`

var gatewayExpect = map[string]string{
	"allowed_http": "ALLOWED", "denied_http": "ALLOWED", "allowed_connect": "ALLOWED", "denied_connect": "ALLOWED",
	"ignores_proxy_by_name": "DENIED", "ignores_proxy_by_ip": "DENIED", "outside_dns": "DENIED",
	"toolbelt": "ALLOWED", "toolbelt_endpoint": "ALLOWED",
}

// TestDockerGateway: from inside a container, an allowed host is reached
// through the gateway's proxy (plain HTTP and CONNECT), a denied host gets
// 403, a tool that ignores the proxy has no route (by name or by address),
// and the os on PATH reaches the execution's toolbelt through the forward,
// in the platform's forward mode or the one forced.
func TestDockerGateway(t *testing.T) {
	l := newDockerLab(t)
	const id = "gw"
	_, socket, tokenFile, token := l.labToolbelt(t, id)
	b := l.toolbeltBinding(l.grant(`,"network":{"allow":["allowed.test"]}`), id, socket)
	res, err := b.Resources(dockerWorkspace{l.ws}).Acquire(context.Background(), extension.ResourceRequest{Sandboxed: true})
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(l.runRoot, id)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	upstreamFile := runDir + "/upstream"
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "OS_ENDPOINT=unix://" + socket, "OS_TOKEN=" + token, "OS_TOKEN_FILE=" + tokenFile}
	plan, err := b.Launch(context.Background(), extension.SandboxNeeds{Sockets: []string{socket}, Hosts: []string{"allowed.test"}, Read: []string{filepath.Dir(tokenFile)}},
		extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", `LAB_UPSTREAM=$(cat "` + upstreamFile + `"); ` + gatewayProbes}, Dir: res.Dir, Env: env, RunDir: runDir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Summary, "gateway allowing allowed.test") || !strings.Contains(plan.Summary, "toolbelt forwarded ("+l.provider.Forward()+")") {
		t.Errorf("summary %q", plan.Summary)
	}
	l.networkOf(t, l.pinned().ID)
	ip := l.upstream(t, "allowed.test", "denied.test")
	if err := os.WriteFile(upstreamFile, []byte(ip), 0o600); err != nil {
		t.Fatal(err)
	}
	w := startWrapper(t, plan, nil)
	if code := w.wait(t); code != 0 {
		t.Fatalf("the probes exited %d:\n%s", code, w.out.String())
	}
	got := probeResults(w.out.String())
	for k, want := range gatewayExpect {
		if got[k] != want {
			t.Errorf("%s: %q from inside the container, want %s", k, got[k], want)
		}
	}
	if t.Failed() {
		name, _ := l.docker("inspect", "--format", "{{.Name}}", dockerID(l.pinned().ID))
		logs, _ := l.docker("logs", strings.TrimPrefix(name, "/")+"-gw")
		t.Logf("probes:\n%s\ngateway:\n%s", w.out.String(), logs)
	}
}

// TestDockerShell: the shell executor runs a binding's command in a
// container end to end: its workspace and toolbelt, a host its grant
// allows, its outputs, and a completed result with the container's exit.
func TestDockerShell(t *testing.T) {
	l := newDockerLab(t)
	const id = "shell"
	_, socket, tokenFile, token := l.labToolbelt(t, id)
	b := l.toolbeltBinding(l.grant(`,"network":{"allow":["allowed.test"]}`), id, socket)
	ex, err := shell.New(shell.Options{StateDir: filepath.Dir(l.runRoot), Environ: func() []string { return []string{"PATH=/usr/bin:/bin", "HOME=" + l.home} }})
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Capabilities().Container {
		t.Fatal("the shell executor does not advertise containers")
	}
	command := `os context > ./context.json
i=0; until wget -q -T 3 -O ./fetched http://allowed.test:8080/; do i=$((i+1)); [ $i -lt 40 ] || exit 9; sleep 0.5; done
echo in-container > "$OS_OUTPUT_DIR/note"
[ "$(id -u)" = "$LAB_UID" ] || exit 8
exit 0`
	config, _ := json.Marshal(map[string]any{"command": command, "env": map[string]string{"LAB_UID": fmt.Sprint(os.Getuid())}})
	h, err := ex.Run(context.Background(), extension.RunRequest{
		Work:      domain.Work{ID: "work", ScopeID: "scope"},
		Execution: domain.Execution{ID: id, Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Context: extension.ExecutionContext{
			Config:           domain.Payload{Data: config},
			Resources:        b.Resources(dockerWorkspace{l.ws}),
			Container:        b,
			ToolbeltEndpoint: "unix://" + socket, ToolbeltToken: token, ToolbeltTokenFile: tokenFile,
		},
		PersistRecovery: func(domain.Payload) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	pin := l.pinned()
	l.networkOf(t, pin.ID)
	l.upstream(t, "allowed.test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r = b.Settle(r)
	logs := ""
	if raw, err := os.ReadFile(filepath.Join(pin.Run, "output.log")); err == nil {
		logs = string(raw)
	}
	if r.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("outcome %+v %s\n%s", r.Outcome, r.Outcome.Data.Data, logs)
	}
	if raw, _ := os.ReadFile(l.ws + "/fetched"); string(raw) != "upstream" {
		t.Errorf("fetched %q through the gateway, want upstream", raw)
	}
	if raw, _ := os.ReadFile(l.ws + "/context.json"); !strings.Contains(string(raw), "the forward") {
		t.Errorf("os context in the container: %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(pin.Run, "outputs", "note")); strings.TrimSpace(string(raw)) != "in-container" {
		t.Errorf("the output written in the container: %q", raw)
	}
	if !strings.HasPrefix(pin.Run, l.runRoot+"/") {
		t.Errorf("the pinned run directory %s is not the shell's", pin.Run)
	}
}
