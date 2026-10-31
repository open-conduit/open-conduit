package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
)

func TestJSONEditKeepsOrderAndAddressesByName(t *testing.T) {
	doc := []byte(`{"version":1,"zeta":{"b":2,"a":1},"scopes":[{"id":"web","bindings":[{"name":"agent","config":{"x":"<y>"}}],"secret_refs":{"t":"env:T"}}]}`)
	root, err := parseNode(doc)
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := pointer("/scopes/web/bindings/agent/config/model")
	if err = root.set(parts, &node{kind: 's', scalar: json.RawMessage(`"opus"`)}); err != nil {
		t.Fatal(err)
	}
	out := string(root.bytes())
	if strings.Index(out, `"zeta"`) > strings.Index(out, `"scopes"`) || strings.Index(out, `"b"`) > strings.Index(out, `"a"`) {
		t.Fatalf("key order changed:\n%s", out)
	}
	if !strings.Contains(out, `"model": "opus"`) || !strings.Contains(out, `"<y>"`) {
		t.Fatalf("edit or escaping wrong:\n%s", out)
	}
	parts, _ = pointer("/zeta/b")
	if err = root.unset(parts); err != nil || strings.Contains(string(root.bytes()), `"b"`) {
		t.Fatalf("unset: %v", err)
	}
	parts, _ = pointer("/scopes/-")
	if err = root.set(parts, &node{kind: 'o'}); err != nil || len(root.field("scopes").arr) != 2 {
		t.Fatalf("append: %v", err)
	}
	root.redact()
	if strings.Contains(string(root.bytes()), "env:T") {
		t.Fatal("reference text not redacted")
	}
	if _, err := parseNode([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

// fakeSetup stands in for an extension's setup: internal/cli never imports
// one (B01), so the host is tested against the public contract.
type fakeSetup struct{ calls int }

func (f *fakeSetup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: "fake", Summary: "a fake scope", Usage: "--repo NAME [--token REF] [--holds]"}
}

func (f *fakeSetup) Setup(_ context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	f.calls++
	var repo, token string
	holds := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			i++
			repo = args[i]
		case "--token":
			i++
			token = args[i]
		case "--holds":
			holds = true
		default:
			return extension.SetupResult{}, fmt.Errorf("%w: unknown flag %s", extension.ErrSetupUsage, args[i])
		}
	}
	if repo == "" {
		return extension.SetupResult{}, fmt.Errorf("%w: init fake needs --repo", extension.ErrSetupUsage)
	}
	if !strings.Contains(token, ":") {
		return extension.SetupResult{}, fmt.Errorf("%w: --token takes a reference", extension.ErrSetupUsage)
	}
	if token == "prompt:" {
		value, err := host.Prompt("Token", extension.Prompt{Secret: true})
		if err != nil {
			return extension.SetupResult{}, err
		}
		if token, err = host.StoreSecret("fake-token", value); err != nil {
			return extension.SetupResult{}, err
		}
	}
	routes := `{"name": "issues", "handler": "fake", "binding": "agent", "active_policy": "queue", "config": {}}`
	if holds {
		routes = `{"name": "held", "handler": "fake-hold", "binding": "agent", "active_policy": "queue", "config": {}}, ` + routes
	}
	return extension.SetupResult{
		Connections: []json.RawMessage{json.RawMessage(`{"id": "github", "type": "fake", "config": {}, "secret_refs": {"token": "` + token + `"}}`)},
		Scope:       json.RawMessage(`{"id": "web", "name": "` + repo + `", "bindings": [{"name": "agent", "executor": "shell", "config": {}}], "routes": [` + routes + `], "secret_refs": {}}`),
		Summary:     []string{"scope web", "workspace  none (workspace-free)"},
		Next:        []string{"open-conduit serve"},
	}, nil
}

func TestMergeSetup(t *testing.T) {
	first := extension.SetupResult{Connections: []json.RawMessage{json.RawMessage(`{"id":"github","type":"github"}`)}, Scope: json.RawMessage(`{"id":"web","bindings":[]}`)}
	out, err := MergeSetup(nil, first, false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Version     int              `json:"version"`
		Connections []map[string]any `json:"connections"`
		Scopes      []map[string]any `json:"scopes"`
	}
	if err = json.Unmarshal(out, &cfg); err != nil || cfg.Version != 1 || len(cfg.Connections) != 1 || len(cfg.Scopes) != 1 || strings.Index(string(out), `"version"`) > strings.Index(string(out), `"scopes"`) {
		t.Fatalf("%v %s", err, out)
	}
	// A second scope merges, keeps the first and reuses the connection.
	second := extension.SetupResult{Connections: []json.RawMessage{json.RawMessage(`{"id":"github","type":"other"}`), json.RawMessage(`{"id":"slack","type":"slack"}`)}, Scope: json.RawMessage(`{"id":"support"}`)}
	out, err = MergeSetup(out, second, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(out, &cfg); err != nil || len(cfg.Connections) != 2 || len(cfg.Scopes) != 2 || cfg.Connections[0]["type"] != "github" {
		t.Fatalf("merge: %v %s", err, out)
	}
	if _, err = MergeSetup(out, second, false); err == nil {
		t.Fatal("replaced an existing scope without --force")
	}
	if _, err = MergeSetup(out, second, true); err != nil {
		t.Fatal(err)
	}
	if _, err = MergeSetup(out, extension.SetupResult{Scope: json.RawMessage(`{"id":"Bad id"}`)}, false); err == nil {
		t.Fatal("invalid scope id accepted")
	}
}

func TestParseClaim(t *testing.T) {
	for in, want := range map[string][2]any{"12": {"", 12}, "#12": {"", 12}, "club#12": {"club", 12}, " web#3 ": {"web", 3}} {
		scope, n, ok := ParseClaim(in)
		if !ok || scope != want[0] || n != want[1] {
			t.Errorf("%q: %q %d %v", in, scope, n, ok)
		}
	}
	for _, in := range []string{"", "#", "x#", "PROJ-12", "web#-1", "Web#2"} {
		if _, _, ok := ParseClaim(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

type setupEnv struct {
	root     string
	env      map[string]string
	stdin    string
	valid    func([]byte) ([]client.Detail, error)
	warnings func(string, []byte) []string
	setup    *fakeSetup
}

func (s *setupEnv) run(args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	if s.setup == nil {
		s.setup = &fakeSetup{}
	}
	e := Env{Getenv: func(k string) string { return s.env[k] }, Cwd: s.root, Stdin: strings.NewReader(s.stdin), ValidateConfig: s.valid, ConfigWarnings: s.warnings, Setups: []extension.Setup{s.setup}}
	code := RunOperator("test", "abc", args, e, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestInitSecretAndConfigEditing(t *testing.T) {
	root := t.TempDir()
	var validated int
	s := &setupEnv{root: root, env: map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "cfg"), "HOME": root}, valid: func(b []byte) ([]client.Detail, error) {
		validated++
		if bytes.Contains(b, []byte(`"concurrency": 0`)) {
			return []client.Detail{{Path: "/settings/concurrency", Code: "schema", Message: "must be at least 1"}}, nil
		}
		return nil, nil
	}}
	path := filepath.Join(root, "cfg", "open-conduit", "config.json")
	// init lists the registered setups; a missing or unknown type is a
	// usage error.
	if out, _, code := s.run("init"); code != 0 || !strings.Contains(out, "fake") || !strings.Contains(out, "a fake scope") {
		t.Fatalf("init listing: %d %s", code, out)
	}
	if _, errOut, code := s.run("init", "--repo", "acme/web"); code != 2 || !strings.Contains(errOut, "TYPE") {
		t.Fatalf("init without type: %d %s", code, errOut)
	}
	if _, errOut, code := s.run("init", "nope"); code != 2 || !strings.Contains(errOut, "nope") {
		t.Fatalf("unknown type: %d %s", code, errOut)
	}
	if _, errOut, code := s.run("init", "fake", "--token", "ghp_literal"); code != 2 || !strings.Contains(errOut, "--repo") {
		t.Fatalf("init without repo: %d %s", code, errOut)
	}
	if _, errOut, code := s.run("init", "fake", "--repo", "acme/web", "--token", "ghp_literal"); code != 2 || !strings.Contains(errOut, "reference") {
		t.Fatalf("literal token accepted: %d %s", code, errOut)
	}
	out, errOut, code := s.run("init", "fake", "--repo", "acme/web", "--token", "file:github-token", "--dry-run")
	if code != 0 || !strings.Contains(out, `"name": "acme/web"`) {
		t.Fatalf("dry run: %d %s %s", code, out, errOut)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("dry run wrote the config")
	}
	out, errOut, code = s.run("init", "fake", "--repo", "acme/web", "--token", "file:github-token", "--holds")
	if code != 0 || !strings.Contains(out, "workspace-free") || !strings.Contains(out, "open-conduit serve") || validated == 0 {
		t.Fatalf("init: %d %s %s", code, out, errOut)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v", err)
	}
	if out, _, code = s.run("config", "path"); code != 0 || strings.TrimSpace(out) != path {
		t.Fatalf("config path: %q", out)
	}
	// Secret values come from stdin, are stored 0600 and never echoed.
	s.stdin = "tok-secret-value\n"
	out, errOut, code = s.run("secret", "set", "github-token", "--store", "file")
	if code != 0 || !strings.Contains(out, "file:github-token") || strings.Contains(out+errOut, "tok-secret-value") {
		t.Fatalf("secret set: %d %s %s", code, out, errOut)
	}
	secrets := filepath.Join(filepath.Dir(path), "secrets.json")
	if b, err := os.ReadFile(secrets); err != nil || !strings.Contains(string(b), `"github-token": "tok-secret-value"`) {
		t.Fatalf("secrets file: %s %v", b, err)
	}
	if info, _ := os.Stat(secrets); info.Mode().Perm() != 0o600 {
		t.Fatal("secrets file not owner-only")
	}
	// Piped input is read whole: a PEM private key keeps every line.
	pemValue := "-----BEGIN RSA PRIVATE KEY-----\nAAAA\nBBBB\n-----END RSA PRIVATE KEY-----"
	s.stdin = pemValue + "\n"
	if out, errOut, code = s.run("secret", "set", "bot-key", "--store", "file"); code != 0 || !strings.Contains(out, "file:bot-key") {
		t.Fatalf("multi-line secret set: %d %s %s", code, out, errOut)
	}
	var stored map[string]string
	if b, err := os.ReadFile(secrets); err != nil || json.Unmarshal(b, &stored) != nil || stored["bot-key"] != pemValue {
		t.Fatalf("multi-line secret stored as %q", stored["bot-key"])
	}
	if _, _, code = s.run("secret", "set", "github-token", "extra"); code != 2 {
		t.Fatal("secret value accepted as an argument")
	}
	// Edits are validated before saving; an invalid one leaves the file.
	before, _ := os.ReadFile(path)
	if _, errOut, code = s.run("config", "set", "/settings/concurrency", "0"); code != 1 || !strings.Contains(errOut, "/settings/concurrency") {
		t.Fatalf("invalid edit: %d %s", code, errOut)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("invalid edit changed the file")
	}
	if _, errOut, code = s.run("config", "set", "/connections/github/secret_refs/token", `"ghp_raw"`); code != 2 || !strings.Contains(errOut, "reference") {
		t.Fatalf("secret value in secret_refs accepted: %d %s", code, errOut)
	}
	if _, errOut, code = s.run("config", "set", "/connections/github/config", `{"api_url":"https://ghe.example/api/v3"}`); code != 0 {
		t.Fatalf("set: %s", errOut)
	}
	out, _, code = s.run("config", "get", "/connections/github")
	if code != 0 || !strings.Contains(out, "ghe.example") || strings.Contains(out, "file:github-token") {
		t.Fatalf("get (redacted): %s", out)
	}
	if out, _, _ = s.run("config", "show"); strings.Contains(out, "file:github-token") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("show not redacted: %s", out)
	}
	if _, errOut, code = s.run("config", "unset", "/scopes/web/routes/held"); code != 0 {
		t.Fatalf("unset: %s", errOut)
	}
	if out, _, _ = s.run("config", "get", "/scopes/web/routes"); strings.Contains(out, "github-hold") {
		t.Fatalf("route not removed: %s", out)
	}
	if out, _, code = s.run("config", "check"); code != 0 || strings.TrimSpace(out) != "valid" {
		t.Fatalf("check: %d %s", code, out)
	}
	// The host's secret prompt stores through the secret store and hands the
	// setup only the reference; a dry run stores nothing.
	restore := keyringStore
	keyringStore = func(string, string) error { return errors.New("no keyring in tests") }
	defer func() { keyringStore = restore }()
	s.stdin = "prompted-value\n"
	out, errOut, code = s.run("init", "fake", "--repo", "acme/web", "--token", "prompt:", "--force", "--dry-run")
	if code != 0 || strings.Contains(out+errOut, "prompted-value") {
		t.Fatalf("dry-run prompt: %d %s %s", code, out, errOut)
	}
	if b, _ := os.ReadFile(secrets); strings.Contains(string(b), "fake-token") {
		t.Fatal("dry run stored a secret")
	}
	s.stdin = "prompted-value\n"
	if out, errOut, code = s.run("init", "fake", "--repo", "acme/web", "--token", "prompt:", "--force"); code != 0 || strings.Contains(out+errOut, "prompted-value") {
		t.Fatalf("prompt: %d %s %s", code, out, errOut)
	}
	if b, _ := os.ReadFile(secrets); !strings.Contains(string(b), `"fake-token": "prompted-value"`) {
		t.Fatalf("prompted secret not stored: %s", b)
	}
}

// config check prints the entrypoint's cmd: warnings without failing.
func TestConfigCheckWarnsMissingCommand(t *testing.T) {
	root := t.TempDir()
	s := &setupEnv{root: root, env: map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "cfg"), "HOME": root}, warnings: func(_ string, data []byte) []string {
		if bytes.Contains(data, []byte("missing-gh-oc-test")) {
			return []string{"cmd: reference runs missing-gh-oc-test, which is not found"}
		}
		return nil
	}}
	path := filepath.Join(root, "cfg", "open-conduit", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(ref string) {
		t.Helper()
		body := `{"connections": [{"id": "github", "secret_refs": {"token": "` + ref + `"}}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cmd:missing-gh-oc-test auth --token hunter2")
	out, errOut, code := s.run("config", "check")
	if code != 0 || strings.TrimSpace(out) != "valid" || errOut != "open-conduit: warning: cmd: reference runs missing-gh-oc-test, which is not found\n" {
		t.Fatalf("check: %d %q %q", code, out, errOut)
	}
	write("cmd:sh -c 'printf x'")
	if _, errOut, code = s.run("config", "check"); code != 0 || errOut != "" {
		t.Fatalf("found command warned: %d %q", code, errOut)
	}
}

// config include adds the current folder (or a named one) under a parent
// scope, refuses a duplicate, and removes it again.
func TestConfigInclude(t *testing.T) {
	root := t.TempDir()
	s := &setupEnv{root: root, env: map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "cfg"), "HOME": root}}
	path := filepath.Join(root, "cfg", "open-conduit", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version": 1, "scopes": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := s.run("config", "include", "add"); code != 2 {
		t.Fatalf("add without --parent: %d", code)
	}
	if _, errOut, code := s.run("config", "include", "add", "--parent", "home"); code != 0 {
		t.Fatalf("add: %d %s", code, errOut)
	}
	if _, _, code := s.run("config", "include", "add", root, "--parent", "home"); code == 0 {
		t.Fatal("a duplicate include was added")
	}
	if _, errOut, code := s.run("config", "include", "add", "web", "--parent", "home"); code != 0 {
		t.Fatalf("add web: %d %s", code, errOut)
	}
	var doc struct{ Includes []map[string]string }
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Includes) != 2 || doc.Includes[0]["path"] != root || doc.Includes[1]["path"] != filepath.Join(root, "web") || doc.Includes[1]["parent"] != "home" {
		t.Fatalf("includes %s", data)
	}
	if _, errOut, code := s.run("config", "include", "remove"); code != 0 {
		t.Fatalf("remove: %d %s", code, errOut)
	}
	if _, _, code := s.run("config", "include", "remove"); code == 0 {
		t.Fatal("removed a folder that is not included")
	}
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Includes) != 1 {
		t.Fatalf("after remove %s", data)
	}
}
