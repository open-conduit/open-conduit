package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// migrate upgrade previews the renamed settings, writes them only with
// --write and once the result validates, and says when nothing is renamed.
func TestMigrateUpgrade(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	old, next := []byte(`{"old": true}`+"\n"), []byte(`{"new": true}`+"\n")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	valid := true
	e := Env{Getenv: func(string) string { return "" }, Cwd: root,
		ValidateConfig: func([]byte) ([]client.Detail, error) {
			if !valid {
				return []client.Detail{{Path: "/x", Code: "schema", Message: "bad"}}, nil
			}
			return nil, nil
		},
		Upgrade: func(b []byte) (MigrateResult, error) {
			if bytes.Equal(b, old) {
				return MigrateResult{Config: next, Items: []MigrateItem{{Level: "changed", Path: "scope eng, posthog entry ph: old", Message: "renamed to new"}}}, nil
			}
			return MigrateResult{Config: b, Items: []MigrateItem{}}, nil
		}}
	run := func(args ...string) (string, int) {
		var out, errOut bytes.Buffer
		code := RunOperator("test", "abc", append([]string{"migrate", "upgrade", "--config", path}, args...), e, &out, &errOut)
		return out.String() + errOut.String(), code
	}
	if out, code := run(); code != 0 || !strings.Contains(out, "renamed to new") || !strings.Contains(out, "Preview") {
		t.Fatalf("preview: %d %s", code, out)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, old) {
		t.Fatal("a preview writes nothing")
	}
	valid = false
	if out, code := run("--write"); code != 1 || !strings.Contains(out, "Not written") {
		t.Fatalf("invalid: %d %s", code, out)
	}
	valid = true
	if out, code := run("--write"); code != 0 || !strings.Contains(out, "Written: "+path) {
		t.Fatalf("write: %d %s", code, out)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, next) {
		t.Fatalf("written %s", b)
	}
	if out, code := run(); code != 0 || !strings.Contains(out, "Nothing to upgrade") {
		t.Fatalf("second run: %d %s", code, out)
	}
}
