package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

// jailProvider plans every launch, recording what it was given.
type jailProvider struct {
	policy extension.SandboxPolicy
	needs  extension.SandboxNeeds
	launch extension.SandboxLaunch
}

func (p *jailProvider) Plan(_ context.Context, _ extension.ResourceOwner, policy extension.SandboxPolicy, needs extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	p.policy, p.needs, p.launch = policy, needs, l
	return extension.SandboxPlan{Argv: append([]string{"jail", "--"}, l.Argv...), Env: l.Env, Signals: extension.SandboxTerm, Summary: "jailed",
		Data: domain.Payload{Schema: "test.jail.v1", Data: json.RawMessage(`{}`)}}, nil
}
func (p *jailProvider) Adopt(context.Context, extension.SandboxPlan, int, int) error { return nil }

// A package's process for an entry is planned with the provider and policy
// the entry's own sandbox names, in a fresh directory beneath the package's
// runs, with the host's protected set denied; an entry with no sandbox, or
// of a type the package declares none for, or in a scope it isn't in, gets
// nothing.
func TestEntrySandboxPlansWithTheEntrysPolicy(t *testing.T) {
	state := t.TempDir()
	jail := &jailProvider{}
	d := &daemon{o: Options{StateDir: state}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.openSandboxes()
	d.sandboxes.providers.set(map[string]extension.SandboxProvider{"jail": jail})
	sb := &config.Sandbox{Provider: "jail", Config: json.RawMessage(`{"network":{"allow":["api.example.com"]}}`)}
	d.publishGeneration(&config.Snapshot{Scopes: map[domain.ID]*config.EffectiveScope{
		"org": {ID: "org", Enabled: true, Extensions: []config.Extension{
			{Name: "crm", Type: "bridge-stdio", Sandbox: sb},
			{Name: "bare", Type: "bridge-stdio"},
			{Name: "other", Type: "other-stdio", Sandbox: sb},
		}},
	}}, 1)
	m := protocol.Manifest{Name: "bridge", Provides: protocol.Provides{Extensions: []protocol.ExtensionDecl{{Type: "bridge-stdio", Sandbox: protocol.EntrySandboxRequired}}}}
	plan := d.entrySandbox(m)

	got, dir, err := plan(context.Background(), "org", "crm", extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"server"}, Env: []string{"TOKEN=t"}, Dir: "/elsewhere", RunDir: "/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(state, "entries", "bridge", "runs")
	if filepath.Dir(dir) != runs || !strings.HasPrefix(filepath.Base(dir), "crm-") {
		t.Fatalf("dir = %s, want a fresh directory in %s", dir, runs)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the process's directory wasn't made: %v", err)
	}
	if string(jail.policy.Config.Data) != string(sb.Config) || extension.CanonicalPath(jail.launch.Dir) != extension.CanonicalPath(dir) || extension.CanonicalPath(jail.launch.RunDir) != extension.CanonicalPath(dir) {
		t.Fatalf("the provider was given %+v / %+v", jail.policy, jail.launch)
	}
	if !slices.Contains(jail.needs.Deny, extension.CanonicalPath(state)) {
		t.Fatalf("the state directory isn't denied: %v", jail.needs.Deny)
	}
	if got.Digest == "" || !slices.Contains(got.Argv, "server") {
		t.Fatalf("plan = %+v", got)
	}
	for _, c := range []struct{ scope, entry string }{{"org", "bare"}, {"org", "other"}, {"org", "missing"}, {"elsewhere", "crm"}} {
		if _, _, err := plan(context.Background(), domain.ID(c.scope), c.entry, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"server"}}); !errors.Is(err, extension.ErrSandboxUnavailable) {
			t.Errorf("%s/%s: %v, want sandbox unavailable", c.scope, c.entry, err)
		}
	}
	// A need into the host's state is refused, and nothing is left behind.
	if _, _, err := plan(context.Background(), "org", "crm", extension.SandboxNeeds{Write: []string{filepath.Join(state, "db")}}, extension.SandboxLaunch{Argv: []string{"server"}}); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Fatalf("a write into the state directory: %v", err)
	}
	if entries, _ := os.ReadDir(runs); len(entries) != 1 {
		t.Fatalf("runs = %d, want only the planned launch's", len(entries))
	}
	// A restarted daemon starts with none.
	d.openSandboxes()
	if _, err := os.Stat(runs); !os.IsNotExist(err) {
		t.Fatalf("runs survived a restart: %v", err)
	}
}
