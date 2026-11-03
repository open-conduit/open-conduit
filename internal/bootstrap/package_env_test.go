package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

func grantConfig(names string) []byte {
	return []byte(`{"version":1,"settings":{"extensions":{"ssh":{"pass_env":[` + names + `]}}},"connections":[],
		"scopes":[{"id":"a","name":"A","authorization":{"connections":[],"operations":[],"read_scopes":[]},"secret_refs":{"gh":"env:GH_TOKEN"}}]}`)
}

// TestPackageGrantSources: packages start before the configuration opens,
// so on a first start the daemon takes their grant from the config file
// (never a name validation would refuse), once a generation is committed
// from that generation rather than an edited file, and once a generation
// is published from the generation itself. A package that isn't granted
// gets nothing (the negative control).
func TestPackageGrantSources(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	path := filepath.Join(dir, "config.json")
	write := func(data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(grantConfig(`"SSH_AUTH_SOCK","OS_TOKEN","GH_TOKEN"`))
	d := &daemon{coord: store, now: now, o: Options{ConfigPath: path}}

	d.external.startGrants = d.startGrants(ctx)
	if got := d.packagePassEnv("ssh"); !slices.Equal(got, []string{"SSH_AUTH_SOCK"}) {
		t.Fatalf("first start grant %v, want SSH_AUTH_SOCK alone", got)
	}
	if got := d.packagePassEnv("docker"); len(got) != 0 {
		t.Fatalf("an ungranted package got %v", got)
	}

	write(grantConfig(`"SSH_AUTH_SOCK"`))
	if _, err := management.OpenConfig(ctx, store, now, config.Registry{}, path, nil); err != nil {
		t.Fatal(err)
	}
	write(grantConfig(`"DOCKER_HOST"`))
	if got := d.startGrants(ctx)["ssh"]; !slices.Equal(got, []string{"SSH_AUTH_SOCK"}) {
		t.Fatalf("restart grant %v, want the committed generation's", got)
	}

	s, err := config.Validate(grantConfig(`"HTTPS_PROXY"`), config.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	d.publishGeneration(s, 2)
	if got := d.packagePassEnv("ssh"); !slices.Equal(got, []string{"HTTPS_PROXY"}) {
		t.Fatalf("published grant %v, want the generation's", got)
	}
}

// TestGrantItem: doctor names the variables a package was granted, those
// granted but unset, and a grant that changed since it started; never a
// value.
func TestGrantItem(t *testing.T) {
	environ := []string{"SSH_AUTH_SOCK=/secret/agent.sock", "DOCKER_HOST=tcp://d"}
	item, pending := grantItem([]string{"SSH_AUTH_SOCK"}, []string{"SSH_AUTH_SOCK", "UNSET"}, environ)
	if pending || !strings.Contains(item, "pass_env SSH_AUTH_SOCK") || !strings.Contains(item, "not set in the daemon's environment: UNSET") || strings.Contains(item, "/secret") {
		t.Fatalf("item %q pending %v", item, pending)
	}
	item, pending = grantItem([]string{"SSH_AUTH_SOCK"}, []string{"DOCKER_HOST"}, environ)
	if !pending || !strings.Contains(item, "restart the daemon") {
		t.Fatalf("a changed grant: item %q pending %v", item, pending)
	}
	if item, pending := grantItem(nil, nil, environ); item != "" || pending {
		t.Fatalf("no grant: item %q pending %v", item, pending)
	}
}
