package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/executors/claudecode"
	"github.com/alsiola/open-conduit/extension"
	gitws "github.com/alsiola/open-conduit/extensions/git"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/extensions/github/playbooks"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
)

// The setup guide's example configuration is a valid Open Conduit config
// for the executors, handlers, operations and providers the daemon
// registers (bootstrap's registry), so an agent following it writes one
// that `config validate` accepts.
func TestSetupGuideExampleConfigValidates(t *testing.T) {
	g := playbooks.Setup("open-conduit")
	start, end := strings.Index(g, "```json\n"), strings.Index(g, "\n```\n\nOther `github`")
	if start < 0 || end < 0 {
		t.Fatal("no example config in the guide")
	}
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir := filepath.Join(t.TempDir(), "claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	claude, err := claudecode.New(claudecode.Options{StateDir: dir, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	reg := config.Registry{
		Executors:      map[string]extension.Executor{"claude-code": claude, gh.HoldExecutor: gh.NewHoldExecutor()},
		Handlers:       map[string]bool{"manual": true, gh.Type: true, gh.HoldHandler: true},
		Extensions:     map[string]config.ExtensionType{gh.Type: {Ingress: true, Validate: gh.ValidateExtension}},
		Connections:    map[string]func(json.RawMessage) error{gh.Type: gh.ValidateConnection},
		Operations:     gh.Operations(),
		Resources:      map[string]bool{gitws.Type: true},
		ResourceConfig: map[string]func(json.RawMessage) error{gitws.Type: gitws.ValidateConfig},
	}
	if _, err := config.Validate([]byte(g[start+8:end]), reg); err != nil {
		t.Fatalf("the setup guide's example config is invalid: %v", err)
	}
}
