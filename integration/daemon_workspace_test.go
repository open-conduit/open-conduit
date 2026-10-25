package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
)

// workspaceScript records where and with what it ran. ws.slow runs until
// released, so a daemon kill finds it alive; ws.fail fails.
const workspaceScript = `set -e
tag="$OS_TRIGGER_TYPE"
pwd -P > "$DRILL_DIR/$tag.pwd"
cat a.txt > "$DRILL_DIR/$tag.a" 2>/dev/null || true
cat b.txt > "$DRILL_DIR/$tag.b" 2>/dev/null || true
printf '%s\n' "${DOTENV_VALUE:-}" > "$DRILL_DIR/$tag.env"
case "$tag" in
ws.slow)
  echo $$ > "$DRILL_DIR/pid"
  while [ ! -e "$DRILL_DIR/release" ]; do sleep 0.05; done ;;
ws.fail) exit 3 ;;
esac
`

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workspaceRepo is a local bare origin, the scope's main checkout at v1 and a
// later v2 of a.txt pushed from elsewhere. Nothing leaves the machine.
func workspaceRepo(t *testing.T, root string) string {
	origin, checkout, other := filepath.Join(root, "origin.git"), filepath.Join(root, "checkout"), filepath.Join(root, "other")
	gitCmd(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitCmd(t, root, "clone", "-q", origin, checkout)
	gitCmd(t, checkout, "checkout", "-q", "-B", "main")
	for name, body := range map[string]string{"a.txt": "v1", "b.txt": "v1"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, checkout, "add", ".")
	gitCmd(t, checkout, "commit", "-qm", "one")
	gitCmd(t, checkout, "push", "-q", "origin", "main")
	gitCmd(t, root, "clone", "-q", origin, other)
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "commit", "-qam", "two")
	gitCmd(t, other, "push", "-q", "origin", "HEAD:main")
	if err := os.WriteFile(filepath.Join(checkout, ".env"), []byte("DOTENV_VALUE=from-dotenv\nOS_TOKEN=forged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return checkout
}

// workspaceConfig binds a fresh-workspace scope, a shared-checkout scope and a
// scope with no resource at all, all on the shell executor.
func workspaceConfig(root, checkout string) []byte {
	binding := func(resource map[string]any) map[string]any {
		b := map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"workspace": resource != nil}, "operations": []string{"read_context"}, "secret_names": []any{},
			"config": map[string]any{"command": workspaceScript, "env": map[string]string{"DRILL_DIR": root}}}
		if resource != nil {
			b["resource"] = map[string]any{"provider": "git", "config": resource}
		}
		return b
	}
	scope := func(id string, resource map[string]any) map[string]any {
		return map[string]any{
			"id": id, "name": id,
			"authorization": map[string]any{"connections": []any{}, "operations": []string{"read_context"}, "read_scopes": []any{}},
			"bindings":      []any{binding(resource)},
			"routes":        []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"secret_refs":   map[string]any{},
		}
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{}}, "connections": []any{},
		"scopes": []any{
			scope("fresh", map[string]any{"checkout": checkout, "mode": "fresh", "copy": []string{".env"}, "dotenv": true, "setup": "echo ran >> setup.log", "retain_failed_seconds": 3600}),
			scope("shared", map[string]any{"checkout": checkout}),
			scope("plain", nil),
		},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonGitWorkspaces drives the composed daemon with the registered git
// provider: a fresh run executes at the remote head in its own worktree with
// the copied .env layer and setup, survives a daemon SIGKILL by adoption and
// is cleaned up after success; a failed fresh run is retained; a shared run
// fast-forwards the main checkout while keeping an unrelated local edit; and a
// scope without any resource still runs.
func TestDaemonGitWorkspaces(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	d := newDrill(t)
	checkout := workspaceRepo(t, d.root)
	if err := os.WriteFile(d.config, workspaceConfig(d.root, checkout), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaces := filepath.Join(d.state, "resources", "git", "workspaces")
	listed := func() []string {
		entries, _ := os.ReadDir(workspaces)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	submit := func(scope, typ, key string) {
		t.Helper()
		if _, err := d.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: scope, Type: typ, DeliveryID: key, CorrelationKey: &client.Key{Source: "ws", ExternalID: key}}, "submit-"+key); err != nil {
			t.Fatal(err)
		}
	}
	work := func(scope string) client.Work {
		page, err := d.c.Works(context.Background(), url.Values{"scope_id": {scope}})
		if err != nil || len(page.Items) != 1 {
			return client.Work{}
		}
		return page.Items[0]
	}
	d.start("")

	// Fresh: start a slow run, kill the daemon while it runs in its worktree.
	submit("fresh", "ws.slow", "fresh-slow")
	pidFile := filepath.Join(d.root, "pid")
	d.until("fresh run started", 30*time.Second, func() bool { return fileHasLine(pidFile) })
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	ran := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.slow.pwd")))
	if !strings.HasPrefix(ran, mustReal(t, workspaces)) {
		t.Fatalf("fresh run executed in %s, not a provider workspace", ran)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.slow.a"))); got != "v2" {
		t.Fatalf("fresh workspace a.txt = %q, want the remote head", got)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.slow.env"))); got != "from-dotenv" {
		t.Fatalf("dotenv layer = %q", got)
	}
	if readFile(t, filepath.Join(ran, "setup.log")) != "ran\n" || readFile(t, filepath.Join(ran, ".env")) == "" {
		t.Fatal("setup did not run or .env was not copied")
	}
	if strings.TrimSpace(readFile(t, filepath.Join(checkout, "a.txt"))) != "v1" {
		t.Fatal("a fresh run moved the main checkout")
	}
	d.kill()
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the fresh run did not survive the daemon")
	}
	d.start("")
	time.Sleep(drillLease) // the dead daemon's lease must expire before adoption
	if !exists(ran) {
		t.Fatal("the running fresh workspace was removed across the restart")
	}
	if err := os.WriteFile(filepath.Join(d.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.until("fresh Work completed", 60*time.Second, func() bool { return work("fresh").State == "completed" })
	d.until("fresh workspace removed after success", 30*time.Second, func() bool { return !exists(ran) })
	if out := gitCmd(t, checkout, "worktree", "list"); strings.Contains(out, filepath.Base(ran)) {
		t.Fatalf("worktree still registered: %s", out)
	}
	execs := d.executions(work("fresh").ID)
	if len(execs) != 1 {
		t.Fatalf("fresh run launched %d attempts across the restart", len(execs))
	}

	// A failed fresh run keeps its workspace for inspection.
	submit("fresh", "ws.fail", "fresh-fail")
	resourcesCheck := func() client.Check {
		report, err := d.c.Doctor(context.Background(), false)
		if err == nil {
			for _, c := range report.Checks {
				if c.Name == "resources" {
					return c
				}
			}
		}
		return client.Check{}
	}
	d.until("failed run retained", 60*time.Second, func() bool {
		c := resourcesCheck()
		return c.Code == "retained" && len(c.Items) == 1 && len(listed()) == 1
	})
	if got := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.fail.a"))); got != "v2" {
		t.Fatalf("failed run a.txt = %q", got)
	}

	// Shared: the main checkout fast-forwards; an unrelated local edit stays.
	if err := os.WriteFile(filepath.Join(checkout, "b.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	submit("shared", "ws.shared", "shared-1")
	d.until("shared Work completed", 60*time.Second, func() bool { return work("shared").State == "completed" })
	if strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.shared.pwd"))) != mustReal(t, checkout) {
		t.Fatal("shared run did not execute in the main checkout")
	}
	if a, b := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.shared.a"))), strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.shared.b"))); a != "v2" || b != "mine" {
		t.Fatalf("shared checkout a=%q b=%q", a, b)
	}
	x := d.executions(work("shared").ID)[0]
	if x.Outcome == nil || x.Outcome.Data == nil || !strings.Contains(string(x.Outcome.Data.Data), `"updated":1`) || !strings.Contains(string(x.Outcome.Data.Data), `"schema":"open-conduit.git.workspace.v1"`) {
		t.Fatalf("shared run did not report the catch-up: %s", x.Outcome.Data.Data)
	}

	// A scope with no resource provider runs as before, with no .env layer.
	submit("plain", "ws.plain", "plain-1")
	d.until("plain Work completed", 60*time.Second, func() bool { return work("plain").State == "completed" })
	if got := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.plain.env"))); got != "" {
		t.Fatalf("a scope without a resource got a provider layer: %q", got)
	}
	if strings.HasPrefix(strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.plain.pwd"))), mustReal(t, checkout)) {
		t.Fatal("a scope without a resource ran in the checkout")
	}
	if c := resourcesCheck(); c.Code != "retained" || len(c.Items) != 1 {
		t.Fatalf("doctor resources check: %+v", c)
	}
}

func mustReal(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
