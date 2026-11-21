package integration_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// TestDockerGatewayContract holds the image's documented `serve` flags
// (architecture/sandboxes.md) for a provider that runs it without this
// module: every flag is passed, and an allowed host connects, a host off
// the list is refused and a listed name resolving to a private address is
// refused. It runs the image built from this tree, what the next release
// publishes, or OPEN_CONDUIT_GATEWAY_CONTRACT_IMAGE (a published digest).
func TestDockerGatewayContract(t *testing.T) {
	l := newDockerLab(t)
	image := dockerGateway
	if v := os.Getenv("OPEN_CONDUIT_GATEWAY_CONTRACT_IMAGE"); v != "" {
		image = v
	}
	if out, err := l.docker("run", "--rm", image, "serve", "--no-such-flag", "x"); err == nil {
		t.Fatalf("an unknown flag was accepted: %s", out)
	}
	egress := l.egress(t, "contract", 197)
	l.upstream(t, egress, "allowed.test", "other.test")
	inner := l.host + "-private"
	if out, err := l.docker("network", "create", "--driver", "bridge", "--ipv6=false", "--subnet", "10.250.197.0/24",
		"--label", extension.ContainerLabelHost+"="+l.host, inner); err != nil {
		t.Fatalf("private network: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = l.docker("network", "rm", inner) })
	l.upstream(t, inner, "private.test")
	name := l.host + "-contract-gw"
	if out, err := l.docker("create", "--name", name, "--label", extension.ContainerLabelHost+"="+l.host, "--network", egress, "--tmpfs", "/run/gw",
		image, "serve", "--listen", ":3199", "--allow", "allowed.test:8080", "--allow", "private.test:8080",
		"--socket", "/run/gw/toolbelt.sock", "--forward", "tcp:/run/gw/port", "--forward-host", "toolbelt.test"); err != nil {
		t.Fatalf("creating the gateway: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = l.docker("rm", "-f", name) })
	if out, err := l.docker("network", "connect", inner, name); err != nil {
		t.Fatalf("connecting the gateway: %v %s", err, out)
	}
	if out, err := l.docker("start", name); err != nil {
		t.Fatalf("starting the gateway: %v %s", err, out)
	}
	script := `for h in allowed.test other.test private.test; do
  i=0; while [ $i -lt 20 ]; do r=$(wget -S -T 3 -O /dev/null "http://$h:8080/" 2>&1 | grep -o 'HTTP/1.[01] [0-9]*' | head -n 1 | cut -d' ' -f2); [ -n "$r" ] && break; i=$((i+1)); sleep 0.25; done
  echo "$h $r"
done`
	out, err := l.docker("run", "--rm", "--network", egress, "-e", fmt.Sprintf("http_proxy=http://%s:3199", name), "--entrypoint", "/bin/sh", dockerTestImage, "-c", script)
	if err != nil {
		logs, _ := l.docker("logs", name)
		t.Fatalf("the client: %v %s\n%s", err, out, logs)
	}
	for _, want := range []string{"allowed.test 200", "other.test 403", "private.test 403"} {
		if !strings.Contains(out, want) {
			logs, _ := l.docker("logs", name)
			t.Errorf("want %q in:\n%s\ngateway:\n%s", want, out, logs)
		}
	}
}
