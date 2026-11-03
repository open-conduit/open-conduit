package extproc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

func TestSanitizedEnvironment(t *testing.T) {
	got := sanitized([]string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "GITHUB_TOKEN=x", "OPEN_CONDUIT_CONFIG=/c", "OS_TOKEN=t", "AWS_SECRET_ACCESS_KEY=k", "TMPDIR=/t", "broken"})
	want := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "TMPDIR=/t"}
	if !slices.Equal(got, want) {
		t.Fatalf("sanitized %v, want %v", got, want)
	}
}

// TestGrantedEnvironment: the operator's grant adds the daemon variables it
// names that are set, on top of the baseline, never a reserved one; an
// ungranted variable stays out (the negative control).
func TestGrantedEnvironment(t *testing.T) {
	environ := []string{"PATH=/bin", "SSH_AUTH_SOCK=/s", "DOCKER_HOST=d", "OS_TOKEN=t", "GITHUB_TOKEN=x"}
	got := Environment(environ, []string{"SSH_AUTH_SOCK", "OS_TOKEN", "UNSET", "PATH", "SSH_AUTH_SOCK"})
	if want := []string{"PATH=/bin", "SSH_AUTH_SOCK=/s"}; !slices.Equal(got, want) {
		t.Fatalf("environment %v, want %v", got, want)
	}
	_, passed := granted(sanitized(environ), environ, []string{"SSH_AUTH_SOCK", "OS_TOKEN", "UNSET", "PATH"})
	if want := []string{"PATH", "SSH_AUTH_SOCK"}; !slices.Equal(passed, want) {
		t.Fatalf("passed %v, want %v", passed, want)
	}
	if got := Environment(environ, nil); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Fatalf("without a grant %v, want the baseline", got)
	}
}

func TestStderrLogIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	l := &boundedLog{f: f, max: 100}
	for i := 0; i < 50; i++ {
		_, _ = l.Write([]byte("0123456789\n"))
	}
	_ = f.Close()
	info, _ := os.Stat(path)
	if info.Size() > 100 {
		t.Fatalf("log grew to %d bytes", info.Size())
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "[log truncated") {
		t.Fatalf("truncation not marked: %q", raw)
	}
}

func TestManifestComparison(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{1}, Name: "chat", Version: "1", Command: []string{"./chat"}}
	raw, _ := json.Marshal(m)
	if !sameManifest(raw, m) {
		t.Fatal("identical manifest refused")
	}
	// Field order and whitespace do not matter; content does.
	if !sameManifest([]byte(` {"version":"1","name":"chat","command":["./chat"],"protocol":[1],"provides":{}} `), m) {
		t.Fatal("reordered manifest refused")
	}
	m2 := m
	m2.Version = "2"
	if sameManifest(raw, m2) {
		t.Fatal("different manifest accepted")
	}
	if sameManifest([]byte(`{"name":"chat","storage":"x"}`), m) {
		t.Fatal("invalid manifest accepted")
	}
}

func TestConfigureResultsStayInTheNamespace(t *testing.T) {
	p := &Process{o: Options{Found: Found{Manifest: protocol.Manifest{Name: "chat", Provides: protocol.Provides{Families: []protocol.Family{{Type: "chat.message.posted", ReDerivable: true}}}}}}, name: "chat"}
	ok := protocol.ConfigureResult{Sources: []protocol.SourceDecl{{Name: "chat/acct", Source: protocol.Source{Namespace: "chat/acct", ScopeID: "s"}, Families: []protocol.EventFamily{{Type: "chat.message.posted", ReDerivable: true}}}}, WebhookPaths: []string{"/chat/events"}}
	if err := p.checkSources(ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]protocol.ConfigureResult{
		"foreign_name":    {Sources: []protocol.SourceDecl{{Name: "github/x", Source: protocol.Source{Namespace: "chat/x"}}}},
		"foreign_source":  {Sources: []protocol.SourceDecl{{Name: "chat/x", Source: protocol.Source{Namespace: "github/x"}}}},
		"family_mismatch": {Sources: []protocol.SourceDecl{{Name: "chat/x", Source: protocol.Source{Namespace: "chat/x"}, Families: []protocol.EventFamily{{Type: "chat.message.posted", ReDerivable: false, Loss: "x"}}}}},
		"undeclared":      {Sources: []protocol.SourceDecl{{Name: "chat/x", Source: protocol.Source{Namespace: "chat/x"}, Families: []protocol.EventFamily{{Type: "chat.other", ReDerivable: true}}}}},
		"webhook":         {WebhookPaths: []string{"/github/events"}},
	} {
		if err := p.checkSources(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
