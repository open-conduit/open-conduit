package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// noProcessExecutor declares that it runs no process, so a scope that
// requires isolation takes its bindings unconfined.
type noProcessExecutor struct{}

func (noProcessExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: extension.ToolbeltSDK, NoProcess: true}
}
func (noProcessExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, errors.New("unused")
}
func (noProcessExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}

// TestConfigIsolation: the launch path's answer comes from the current
// config: the scope that set the requirement for a scope that inherits it,
// none for a scope without it (the control), and a refusal for a binding the
// config no longer has.
func TestConfigIsolation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	path := filepath.Join(dir, "config.json")
	auth := `"authorization":{"connections":[],"operations":[],"read_scopes":[]%s}`
	binding := `"bindings":[{"name":"chat","executor":"chat","requires":{"toolbelt":"sdk"}}]`
	data := `{"version":1,"settings":{},"connections":[],"scopes":[` +
		`{"id":"eng","name":"Eng",` + fmt.Sprintf(auth, `,"isolation":"required"`) + `,` + binding + `},` +
		`{"id":"team","name":"Team","parent":"eng",` + fmt.Sprintf(auth, ``) + `},` +
		`{"id":"open","name":"Open",` + fmt.Sprintf(auth, ``) + `,` + binding + `}]}`
	if err = os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := config.Registry{Executors: map[string]extension.Executor{"chat": noProcessExecutor{}}}
	svc, err := management.OpenConfig(ctx, store, func() time.Time { return time.Unix(1700000000, 0).UTC() }, reg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(scope, name string) (domain.ID, error) {
		return svc.Isolation(ctx, runtime.AttemptState{ScopeID: domain.ID(scope), Binding: domain.Binding{Name: name}})
	}
	if got, err := ask("team", "chat"); err != nil || got != "eng" {
		t.Fatalf("team: %q %v, want eng", got, err)
	}
	if got, err := ask("open", "chat"); err != nil || got != "" {
		t.Fatalf("open: %q %v, want none", got, err)
	}
	if _, err := ask("eng", "gone"); !errors.Is(err, config.ErrDenied) {
		t.Fatalf("a removed binding: %v", err)
	}
}
