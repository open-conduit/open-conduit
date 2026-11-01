package integration_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest"
	"github.com/open-conduit/open-conduit/extensiontest/devscope"
)

// TestDev runs `open-conduit dev` on the notes example: it builds the
// package, starts a daemon on a temporary config and state with the scope
// generated from the manifest and serves the web view. A Work run by a
// scripted agent calls the package's operation; the package's page loads;
// a change to its source rebuilds it and restarts the daemon on the same
// state; and an interrupt stops everything, leaving no process or
// directory behind.
func TestDev(t *testing.T) {
	bin := daemonBinary(t)
	dir := t.TempDir()
	service := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(service, []byte(`{"notes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claude, err := extensiontest.FakeClaude(filepath.Join(dir, "agent"), extensiontest.Turn{
		Calls: []extensiontest.Call{{Operation: "notes.lookup", Input: map[string]any{"id": "missing"}, RequestID: "lookup"}},
		State: "finished",
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := filepath.Abs("../protocol/examples/notes")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "dev", pkg, "--agent", claude.Path, "--watch-interval", "50ms",
		"--entry", `notes={"service":"`+service+`","scope":"test"}`, "--secret", "api_key=test-key")
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "claude-config"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	lines := make(chan string, 100)
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			lines <- s.Text()
		}
		close(lines)
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	var seen []string
	// await reads dev's output until a line contains want.
	await := func(want string) string {
		t.Helper()
		timeout := time.After(3 * time.Minute)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("dev exited before printing %q:\n%s\n%s", want, strings.Join(seen, "\n"), stderr.String())
				}
				seen = append(seen, l)
				if strings.Contains(l, want) {
					return l
				}
			case <-timeout:
				t.Fatalf("dev never printed %q:\n%s\n%s", want, strings.Join(seen, "\n"), stderr.String())
			}
		}
	}

	login := regexp.MustCompile(`http://127\.0\.0\.1:\d+/login\?token=[0-9a-f]+`).FindString(await("        or: "))
	page := strings.Fields(await("Page:"))[1]
	state := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(await("State:")), "State:"))
	await("Watching")
	if login == "" || !strings.HasSuffix(page, "/pages/notes") || !strings.HasPrefix(state, "/") {
		t.Fatalf("dev's output:\n%s", strings.Join(seen, "\n"))
	}

	// The scripted agent's call reaches the package (which has no such
	// note) through the scope generated from its manifest.
	c := devClient(t, state)
	ctx := context.Background()
	out, err := c.CreateWork(ctx, client.WorkRequest{ScopeID: devscope.ScopeID, Kind: "notes.note", Binding: devscope.AgentBinding}, "dev-test-1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Work to complete", func() bool {
		w, err := c.Work(ctx, out.Work.ID)
		return err == nil && w.State == "completed"
	})
	if r, ok := claude.Turn(1); !ok || len(r.Results) != 1 || !r.Results[0].IsError || !strings.Contains(r.Results[0].Text, "no note missing") {
		t.Fatalf("the agent's lookup: %+v", r)
	}

	// The web view signs in and shows the package's page.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	if resp, err := browser.Get(login); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in: %v %v", resp, err)
	}
	resp, err := browser.Get(page)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the notes page: %v %v", resp, err)
	}
	resp.Body.Close()

	// A change to the package's files rebuilds it and restarts the daemon
	// on the same state. Only the source's modification time changes, and
	// it is put back.
	src := filepath.Join(pkg, "notes.go")
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chtimes(src, fi.ModTime(), fi.ModTime()) })
	later := fi.ModTime().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	await("changed: rebuilding notes")
	await("Restarted the daemon with notes")
	c = devClient(t, state)
	if w, err := c.Work(ctx, out.Work.ID); err != nil || w.State != "completed" {
		t.Fatalf("the Work after the restart: %+v %v", w, err)
	}

	// An interrupt stops the daemon and everything it started, and
	// removes the directory.
	started := devscope.Descendants(cmd.Process.Pid)
	if len(started) == 0 {
		t.Fatal("dev runs no daemon")
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	await("Stopped the daemon and removed")
	select {
	case <-exited:
	case <-time.After(time.Minute):
		t.Fatal("dev did not exit")
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("dev exited %d:\n%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Dir(state)); !os.IsNotExist(err) {
		t.Fatalf("dev left %s: %v", filepath.Dir(state), err)
	}
	waitFor(t, "every process dev started to exit", func() bool { return len(devscope.Alive(started)) == 0 })
}

// devClient is the operator's client for dev's daemon.
func devClient(t *testing.T, state string) *client.Client {
	t.Helper()
	f, err := client.ReadTokenFile(filepath.Join(state, "private", "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New("unix://"+filepath.Join(state, "management.sock"), f.Token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
