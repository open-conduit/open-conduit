package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
)

// diskSafetyConfig binds a work-workspace scope whose run fails, and a fresh
// scope whose resource keeps (almost) the whole disk free, so every new
// workspace is refused without filling a real disk.
func diskSafetyConfig(root, checkout string) []byte {
	scope := func(id string, resource map[string]any) map[string]any {
		return map[string]any{
			"id": id, "name": id,
			"authorization": map[string]any{"connections": []any{}, "operations": []string{"read_context"}, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"workspace": true}, "operations": []string{"read_context"}, "secret_names": []any{},
				"config":   map[string]any{"command": workspaceScript, "env": map[string]string{"DRILL_DIR": root}},
				"resource": map[string]any{"provider": "git", "config": resource}}},
			"routes":      []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{}}, "connections": []any{},
		"scopes": []any{
			scope("work", map[string]any{"checkout": checkout, "mode": "work", "copy": []string{".env"}, "retain_failed_seconds": 86400}),
			scope("full", map[string]any{"checkout": checkout, "mode": "fresh", "disk_full_percent": 0.001, "disk_reserve_bytes": uint64(1) << 60}),
		},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonWorkspaceDiskSafety drives v1 #63 on the composed daemon: a new
// workspace that would leave the disk too full is refused with a structured,
// actionable failure and nothing is made; the periodic sweep removes a
// cancelled Work's clean workspace (inside its retention) and an idle clean
// worktree no lease holds, keeps a dirty one and reports both in doctor.
func TestDaemonWorkspaceDiskSafety(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	d := newDrill(t)
	checkout := workspaceRepo(t, d.root)
	if err := os.WriteFile(d.config, diskSafetyConfig(d.root, checkout), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, checkout, "fetch", "-q", "origin")
	workspaces := filepath.Join(d.state, "resources", "git", "workspaces")
	if err := os.MkdirAll(workspaces, 0o700); err != nil {
		t.Fatal(err)
	}
	// Worktrees no lease holds, idle for two days: one clean, one with an
	// unsaved file.
	orphanClean := filepath.Join(workspaces, "work-"+strings.Repeat("a", 32))
	orphanDirty := filepath.Join(workspaces, "fresh-"+strings.Repeat("b", 32))
	for _, p := range []string{orphanClean, orphanDirty} {
		gitCmd(t, checkout, "worktree", "add", "-q", "--detach", p, "origin/main")
	}
	if err := os.WriteFile(filepath.Join(orphanDirty, "scratch.txt"), []byte("unsaved"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{orphanClean, orphanDirty} {
		if err := os.Chtimes(p, old, old); err != nil {
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
	submit := func(scope, typ, key string) {
		t.Helper()
		if _, err := d.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: scope, Type: typ, DeliveryID: key, CorrelationKey: &client.Key{Source: "ws", ExternalID: key}}, "submit-"+key); err != nil {
			t.Fatal(err)
		}
	}
	resources := func() client.Check {
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
	d.extra = []string{"--sweep-interval", "1s"}
	d.start("")

	// Refusal: a structured failed result naming the shortfall, no workspace.
	submit("full", "ws.full", "full-1")
	var refused client.Execution
	d.until("refused run finished", 60*time.Second, func() bool {
		w := work("full")
		if w.ID == "" {
			return false
		}
		execs := d.executions(w.ID)
		if len(execs) == 1 && execs[0].Outcome != nil {
			refused = execs[0]
			return true
		}
		return false
	})
	out := refused.Outcome
	if out.Kind != "failed" || !strings.Contains(out.Summary, "not enough disk space for a new workspace") || !strings.Contains(out.Summary, "Free space, then retry") ||
		out.Data == nil || !strings.Contains(string(out.Data.Data), `"reason":"workspace_no_space"`) {
		t.Fatalf("refusal outcome: %+v %s", out, out.Data.Data)
	}
	if exists(filepath.Join(d.root, "ws.full.pwd")) {
		t.Fatal("the refused run's command ran")
	}

	// A failed run in a work workspace, then its Work is cancelled: retained
	// for a day by reconcile, but clean, pushed and idle, so the sweep takes it.
	submit("work", "ws.fail", "work-1")
	d.until("work run failed", 60*time.Second, func() bool {
		w := work("work")
		execs := d.executions(w.ID)
		return len(execs) == 1 && execs[0].Outcome != nil
	})
	ran := strings.TrimSpace(readFile(t, filepath.Join(d.root, "ws.fail.pwd")))
	if !strings.HasPrefix(ran, mustReal(t, workspaces)) {
		t.Fatalf("work run executed in %s", ran)
	}
	ref := filepath.Base(ran)
	w := work("work")
	if _, err := d.c.CancelWork(context.Background(), w.ID, w.Revision, "drill", "cancel-work-1"); err != nil {
		t.Fatal(err)
	}
	d.until("sweep removed the finished and orphaned workspaces", 60*time.Second, func() bool {
		c := resources()
		return slices.Contains(c.Items, "swept work "+ref+" finished_work") && slices.Contains(c.Items, "swept work "+filepath.Base(orphanClean)+" orphaned")
	})
	c := resources()
	if !slices.Contains(c.Items, "kept fresh "+filepath.Base(orphanDirty)+" orphaned: uncommitted") || c.Code != "spare_kept" || !strings.Contains(c.Detail, "the sweep at") {
		t.Fatalf("doctor resources after the sweep: %+v", c)
	}
	if exists(ran) || exists(orphanClean) || !exists(orphanDirty) {
		t.Fatalf("after the sweep: finished %v, orphan %v, dirty orphan %v", exists(ran), exists(orphanClean), exists(orphanDirty))
	}
	list := gitCmd(t, checkout, "worktree", "list")
	if strings.Contains(list, ref) || strings.Contains(list, filepath.Base(orphanClean)) {
		t.Fatalf("swept worktrees still registered: %s", list)
	}
	if !strings.Contains(gitCmd(t, checkout, "branch", "--list", "agent/work-*"), "agent/work-") {
		t.Fatal("the swept Work's branch was deleted")
	}
}
