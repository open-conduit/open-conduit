package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/linear/lineartest"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// demandConfig uses five packages: the shell executor, and entries of the
// human, schedule, webhook and observe types. With linear, it adds a
// Linear connection and entry, whose teams are given.
func demandConfig(linearURL string, teams []string) []byte {
	scope := map[string]any{
		"id": "ops", "name": "Operations", "secret_refs": map[string]any{"hook": "env:OC_TEST_HOOK_SECRET"},
		"authorization": map[string]any{"connections": []any{}, "operations": []string{"read_context"}, "read_scopes": []any{}},
		"extensions": []any{
			map[string]any{"name": "decisions", "type": "human", "secret_names": []any{}, "config": map[string]any{}},
			map[string]any{"name": "jobs", "type": "schedule", "secret_names": []any{}, "config": map[string]any{"namespace": "schedule/ops", "scope": "ops",
				"jobs": []any{map[string]any{"name": "tick", "every_minutes": 60}}}},
			map[string]any{"name": "inbox", "type": "webhook", "secret_names": []string{"hook"}, "config": map[string]any{"scope": "ops", "webhook_secret": "hook",
				"hooks": []any{map[string]any{"name": "deploys", "delivery_key": "header:X-Delivery-Id", "correlation_key": "body:/service"}}}},
			map[string]any{"name": "help", "type": "observe", "secret_names": []any{}, "config": map[string]any{"sources": []any{map[string]any{"name": "slack-help", "reporters": []string{"run"}}}}},
		},
		"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"}, "secret_names": []any{},
			"config": map[string]any{"command": "true"}}},
		"routes": []any{
			map[string]any{"name": "deliveries", "handler": "webhook", "binding": "run", "active_policy": "queue", "config": map[string]any{}},
			map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}},
		},
	}
	connections := []any{}
	if teams != nil {
		connections = append(connections, map[string]any{"id": "linear", "type": "linear", "config": map[string]any{"api_url": linearURL}, "secret_refs": map[string]any{"token": "env:OC_TEST_LINEAR_TOKEN"}})
		scope["authorization"].(map[string]any)["connections"] = []string{"linear"}
		scope["extensions"] = append(scope["extensions"].([]any), map[string]any{"name": "eng", "type": "linear", "connection": "linear", "secret_names": []any{},
			"config": map[string]any{"namespace": "linear/eng", "scope": "ops", "teams": teams}})
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "settings": map[string]any{}, "connections": connections, "scopes": []any{scope}})
	return b
}

// packageProcesses are the names of the packages the daemon runs as its
// own children (`open-conduit extension-run NAME`), sorted.
func packageProcesses(t *testing.T, daemon int) []string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if ppid, _ := strconv.Atoi(f[1]); ppid != daemon {
			continue
		}
		if i := slices.Index(f, "extension-run"); i > 0 && i+1 < len(f) {
			names = append(names, f[i+1])
		}
	}
	sort.Strings(names)
	return names
}

// onlyThese says how the running packages differ from want, or nil.
func onlyThese(got []string, want ...string) error {
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("package processes %v, want %v", got, want)
	}
	return nil
}

// TestDaemonStartsOnlyUsedPackages (#734): of the two dozen bundled
// packages, the daemon runs the five its configuration uses. A daemon
// that starts every package (the negative control) fails that check. An
// apply that adds a Linear entry starts the Linear package without a
// restart, and its doctor check appears; while it is stopped, a candidate
// naming it is still validated by it; an apply that removes it stops it.
func TestDaemonStartsOnlyUsedPackages(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	lf := lineartest.New("lin_api_t", "ENG")
	defer lf.Close()
	t.Setenv("OC_TEST_LINEAR_TOKEN", "lin_api_t")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook")
	five := []string{"human", "observe", "schedule", "shell", "webhook"}

	// The negative control: a daemon that starts everything fails the
	// check below.
	t.Setenv("OPEN_CONDUIT_START_ALL_PACKAGES", "1")
	all := newDrill(t)
	if err := os.WriteFile(all.config, demandConfig("", nil), 0o600); err != nil {
		t.Fatal(err)
	}
	all.start("")
	all.until("every package started", 20*time.Second, func() bool { return len(packageProcesses(t, all.cmd.Process.Pid)) > len(five) })
	if err := onlyThese(packageProcesses(t, all.cmd.Process.Pid), five...); err == nil {
		t.Fatal("the check passed a daemon that starts every package")
	}
	all.kill()
	t.Setenv("OPEN_CONDUIT_START_ALL_PACKAGES", "")

	d := newDrill(t)
	if err := os.WriteFile(d.config, demandConfig("", nil), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	pid := d.cmd.Process.Pid
	if err := onlyThese(packageProcesses(t, pid), five...); err != nil {
		t.Fatal(err)
	}
	// What is installed is still listed, and says what runs.
	listed, err := d.c.Extensions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running := map[string]bool{}
	for _, x := range listed {
		running[x.Name] = x.Running
	}
	if _, ok := running["linear"]; !ok || running["linear"] || !running["shell"] || len(listed) <= len(five) {
		t.Fatalf("extensions: %+v", listed)
	}

	// A candidate naming the stopped Linear package is validated by it:
	// it accepts a valid entry and refuses an invalid one, with its
	// diagnostic at the entry's config.
	if v, err := d.c.ValidateConfig(ctx, demandConfig(lf.URL(), []string{})); err == nil || v.Valid || !strings.Contains(err.Error(), "scopes/0/extensions/4/config") || !strings.Contains(err.Error(), "rejected by its registered owner") {
		t.Fatalf("invalid Linear entry: %+v %v", v, err)
	}
	with := demandConfig(lf.URL(), []string{"ENG"})
	v, err := d.c.ValidateConfig(ctx, with)
	if err != nil || !v.Valid {
		t.Fatalf("valid Linear entry: %+v %v", v, err)
	}

	// Applying it starts Linear, without a restart, and doctor checks it.
	if _, err := d.c.ApplyConfig(ctx, with, v.CandidateHash, "1", "add-linear"); err != nil {
		t.Fatal(err)
	}
	d.until("the Linear package started", 20*time.Second, func() bool {
		return onlyThese(packageProcesses(t, pid), append([]string{"linear"}, five...)...) == nil
	})
	doc, err := d.c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(doc.Checks, func(c client.Check) bool { return c.Name == "linear" }) {
		t.Fatalf("no linear doctor check: %+v", doc.Checks)
	}

	// Applying one without it stops the process.
	v, err = d.c.ValidateConfig(ctx, demandConfig("", nil))
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if _, err := d.c.ApplyConfig(ctx, demandConfig("", nil), v.CandidateHash, "2", "remove-linear"); err != nil {
		t.Fatal(err)
	}
	d.until("the Linear package stopped", 20*time.Second, func() bool { return onlyThese(packageProcesses(t, pid), five...) == nil })
	if pid != d.cmd.Process.Pid {
		t.Fatal("the daemon restarted")
	}

	// A package whose last entry goes gets the configure that says so
	// before it stops, so it lets go of what it held: the schedule's job
	// timer, armed while its entry stood, is cancelled.
	d.kill()
	if n := armedTimers(t, d.state); n == 0 {
		t.Fatal("the schedule entry armed no timer")
	}
	d.start("")
	pid = d.cmd.Process.Pid
	without := withoutEntry(t, demandConfig("", nil), "jobs")
	v, err = d.c.ValidateConfig(ctx, without)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if _, err := d.c.ApplyConfig(ctx, without, v.CandidateHash, "3", "remove-schedule"); err != nil {
		t.Fatal(err)
	}
	d.until("the schedule package stopped", 20*time.Second, func() bool {
		return onlyThese(packageProcesses(t, pid), "human", "observe", "shell", "webhook") == nil
	})
	d.kill()
	if n := armedTimers(t, d.state); n != 0 {
		t.Fatalf("%d timer(s) still armed after the schedule entry went", n)
	}
}

// withoutEntry is a config with the extension entry named name removed.
func withoutEntry(t *testing.T, raw []byte, name string) []byte {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	scope["extensions"] = slices.DeleteFunc(scope["extensions"].([]any), func(e any) bool { return e.(map[string]any)["name"] == name })
	out, _ := json.Marshal(cfg)
	return out
}

// armedTimers counts the timers a stopped daemon's store holds.
func armedTimers(t *testing.T, state string) int {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	n := 0
	err = store.Transact(context.Background(), storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		all, err := tx.ListTimers()
		n = len(all)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
