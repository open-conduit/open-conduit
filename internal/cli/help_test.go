package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
)

// helpEnv has no daemon, token or config: a command that tried to reach
// the daemon would fail rather than print help.
func helpEnv(t *testing.T) Env {
	root := t.TempDir()
	env := map[string]string{"HOME": root, "XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_CONFIG_HOME": filepath.Join(root, "config")}
	return Env{Getenv: func(k string) string { return env[k] }, Cwd: root, Setups: []extension.Setup{&fakeSetup{}}}
}

// Every command's --help prints its usage and exits 0 before contacting
// the daemon, wherever the flag comes.
func TestCommandHelpNeedsNoDaemon(t *testing.T) {
	env := helpEnv(t)
	cmds := map[string]bool{}
	for _, g := range helpGroups {
		for _, e := range g.entries {
			cmds[e.cmd] = true
		}
	}
	for cmd := range cmds {
		for _, args := range [][]string{{cmd, "--help"}, {cmd, "-h"}, {cmd, "list", "--help"}, {cmd, "help"}} {
			var out, errOut bytes.Buffer
			if code := RunOperator("t", "c", args, env, &out, &errOut); code != 0 || out.Len() == 0 || errOut.Len() != 0 {
				t.Errorf("%v: exit %d, out %q, err %q", args, code, out.String(), errOut.String())
			}
		}
	}
	// The negative control: without --help the same command does reach for
	// the daemon and says it is not running.
	var out, errOut bytes.Buffer
	if code := RunOperator("t", "c", []string{"work", "list"}, env, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no Open Conduit daemon") {
		t.Fatalf("work list without a daemon: %d %q", code, errOut.String())
	}
}

func TestCommandHelpContent(t *testing.T) {
	env := helpEnv(t)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunOperator("t", "c", args, env, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	// config's help lists the local subcommands and says which need the daemon.
	if _, out, _ := run("config", "--help"); !strings.Contains(out, "config check") || !strings.Contains(out, "config set") || !strings.Contains(out, "config edit") || !strings.Contains(out, "(no daemon needed)") || !strings.Contains(out, "(needs the daemon") {
		t.Errorf("config --help:\n%s", out)
	}
	// work --help gathers its observe and control lines and nothing else.
	if _, out, _ := run("work", "--help"); !strings.Contains(out, "work list") || !strings.Contains(out, "work cancel") || strings.Contains(out, "events emit") {
		t.Errorf("work --help:\n%s", out)
	}
	// init TYPE --help is that setup's usage.
	if code, out, _ := run("init", "fake", "--help"); code != 0 || !strings.Contains(out, "open-conduit init fake --repo NAME") {
		t.Errorf("init fake --help: %d\n%s", code, out)
	}
	// A bare command that needs a subcommand prints its help as a usage error.
	if code, _, errOut := run("events"); code != 2 || !strings.Contains(errOut, "events list") {
		t.Errorf("events: %d %s", code, errOut)
	}
	// The global help is grouped.
	_, out, _ := run("--help")
	for _, g := range []string{"Set up:", "Observe:", "Control:", "Maintain:"} {
		if !strings.Contains(out, "\n"+g+"\n") {
			t.Errorf("global help has no %s group", g)
		}
	}
}

func TestListTable(t *testing.T) {
	var out bytes.Buffer
	o := &operator{out: &out, errOut: &out}
	page := client.Page[client.Work]{Items: []client.Work{
		{ID: "work/1", State: "ready", Kind: "github.issue", ScopeID: "eng", Display: &client.Display{Title: "Fix\nthe   thing"}, UpdatedAt: "2026-11-02T01:00:00Z"},
		{ID: "work/2", State: "waiting", Kind: "manual", ScopeID: "ops"},
	}, NextCursor: "c2"}
	if code := o.table(page, workColumns); code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "TITLE") || !strings.Contains(lines[1], "Fix the thing") || !strings.Contains(lines[2], "-") || lines[3] != "next cursor: c2" {
		t.Fatalf("table:\n%s", out.String())
	}
	// Columns line up: every row's STATE starts where the header's does.
	at := strings.Index(lines[0], "STATE")
	if strings.Index(lines[1], "ready") != at || strings.Index(lines[2], "waiting") != at {
		t.Fatalf("columns misaligned:\n%s", out.String())
	}
	out.Reset()
	o.table([]client.Action{}, actionColumns)
	if out.String() != "none\n" {
		t.Fatalf("empty list: %q", out.String())
	}
}

// --scope and --scope-id are the same flag on every command.
func TestScopeAlias(t *testing.T) {
	for _, names := range [][]string{{"scope-id"}, {"scope"}} {
		for _, arg := range []string{"--scope", "--scope-id"} {
			f := newFlags("x", names...)
			if _, err := f.parse([]string{arg, "eng"}); err != nil || f.get(names[0]) != "eng" {
				t.Errorf("%v with %s: %v %q", names, arg, err, f.get(names[0]))
			}
		}
	}
}
