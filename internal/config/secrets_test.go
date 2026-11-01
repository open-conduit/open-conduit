package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeKeyring map[string]string

func (k fakeKeyring) Get(service, name string) (string, error) {
	if v, ok := k[service+"/"+name]; ok {
		return v, nil
	}
	return "", errors.New("provider says: item hunter2 not found")
}

func secretsFile(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// failure asserts a safe ErrSecret with code and no leaked text.
func failure(t *testing.T, err error, code string, leaks ...string) {
	t.Helper()
	if !errors.Is(err, ErrSecret) || SecretCode(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	for _, s := range leaks {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks %q: %v", s, err)
		}
	}
}

func TestKeyring(t *testing.T) {
	r := &Resolver{Keyring: fakeKeyring{"open-conduit/chat_token": "s3cret"}}
	if v, err := r.Resolve(context.Background(), "keyring:chat_token"); err != nil || v != "s3cret" {
		t.Fatal(v, err)
	}
	_, err := r.Resolve(context.Background(), "keyring:missing")
	failure(t, err, "keyring_unavailable", "missing", "hunter2", "keyring:")
}

func TestSecretsFile(t *testing.T) {
	ctx := context.Background()
	path := secretsFile(t, 0o600, `{"code_token":"tok en","empty":"","number":1}`)
	r := &Resolver{SecretsFile: path}
	if v, err := r.Resolve(ctx, "file:code_token"); err != nil || v != "tok en" {
		t.Fatal(v, err)
	}
	_, err := r.Resolve(ctx, "file:absent")
	failure(t, err, "file_missing", "absent")
	_, err = r.Resolve(ctx, "file:empty")
	failure(t, err, "empty")
	_, err = r.Resolve(ctx, "file:number")
	failure(t, err, "file_invalid")
	_, err = (&Resolver{SecretsFile: secretsFile(t, 0o644, `{"code_token":"x"}`)}).Resolve(ctx, "file:code_token")
	failure(t, err, "file_unavailable")
	_, err = (&Resolver{SecretsFile: secretsFile(t, 0o600, `{"a":"x","a":"y"}`)}).Resolve(ctx, "file:a")
	failure(t, err, "file_invalid")
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err = (&Resolver{SecretsFile: link}).Resolve(ctx, "file:code_token")
	failure(t, err, "file_unavailable")
	_, err = (&Resolver{}).Resolve(ctx, "file:code_token")
	failure(t, err, "file_unavailable")
}

func TestEnv(t *testing.T) {
	env := map[string]string{"SET": "value", "EMPTY": ""}
	r := &Resolver{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	if v, err := r.Resolve(context.Background(), "env:SET"); err != nil || v != "value" {
		t.Fatal(v, err)
	}
	for _, ref := range []string{"env:MISSING", "env:EMPTY"} {
		_, err := r.Resolve(context.Background(), ref)
		failure(t, err, "env_missing", "MISSING", "EMPTY")
	}
}

func TestInvalidReferences(t *testing.T) {
	for _, ref := range []string{"", "literal", "https://example", "vault:x", "env:", "env:1BAD", "keyring:Bad", "file:../x", "cmd:", "cmd:a\nb"} {
		_, err := (&Resolver{}).Resolve(context.Background(), ref)
		failure(t, err, "invalid_reference")
		if ref != "" && strings.Contains(err.Error(), ref) {
			t.Fatal(err)
		}
	}
}

func TestCommand(t *testing.T) {
	ctx := context.Background()
	t.Setenv("OS_TOKEN", "toolbelt-token")
	t.Setenv("OS_ENDPOINT", "unix:///tmp/x")
	t.Setenv("OPEN_CONDUIT_CONFIG", "/tmp/c")
	r := &Resolver{CommandTimeout: 5 * time.Second}
	if v, err := r.Resolve(ctx, `cmd:printf ' a  b \r\n'`); err != nil || v != " a  b " {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := r.Resolve(ctx, `cmd:printf 'x\n\n'`); err != nil || v != "x\n" {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := r.Resolve(ctx, `cmd:echo leaked-stderr >&2; printf ok`); err != nil || v != "ok" {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := r.Resolve(ctx, `cmd:printf '%s|%s|%s|%s' "${OS_TOKEN:-unset}" "${OS_ENDPOINT:-unset}" "${OPEN_CONDUIT_CONFIG:-unset}" "$PATH"`); err != nil || v != "unset|unset|unset|"+CommandPath {
		t.Fatalf("%q %v", v, err)
	}
	cases := []struct{ cmd, code string }{
		{`printf ''`, "empty"},
		{`printf '\n'`, "empty"},
		{`echo secret-output; echo stderr-text >&2; exit 3`, "command_failed"},
		{`head -c 70000 /dev/zero | tr '\0' a`, "output_too_large"},
		{`yes secret-output`, "output_too_large"},
		{`no-such-command-oc-test auth token`, "command_not_found"},
		{`/nonexistent/oc-test auth token`, "command_not_found"},
	}
	for _, c := range cases {
		_, err := r.Resolve(ctx, "cmd:"+c.cmd)
		failure(t, err, c.code, "secret-output", "stderr-text", "printf", "cmd:", c.cmd)
	}
	start := time.Now()
	_, err := (&Resolver{CommandTimeout: 200 * time.Millisecond}).Resolve(ctx, "cmd:sleep 5; printf late")
	failure(t, err, "timeout", "sleep")
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not kill the process group")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = r.Resolve(cancelled, "cmd:printf x")
	if !errors.Is(err, ErrSecret) {
		t.Fatal(err)
	}
}

func TestResolveNames(t *testing.T) {
	scope := &EffectiveScope{SecretRefs: map[string]string{"a": "env:A", "b": "env:B", "bad": "env:MISSING"}}
	r := &Resolver{LookupEnv: func(k string) (string, bool) {
		return map[string]string{"A": "1", "B": "2"}[k], k == "A" || k == "B"
	}}
	got, err := r.ResolveNames(context.Background(), scope, []string{"a"})
	if err != nil || len(got) != 1 || got["a"] != "1" {
		t.Fatal(got, err)
	}
	again, _ := r.ResolveNames(context.Background(), scope, []string{"a"})
	again["a"] = "mutated"
	if got["a"] != "1" {
		t.Fatal("result maps are shared")
	}
	_, err = r.ResolveNames(context.Background(), scope, []string{"a", "removed"})
	failure(t, err, "unknown_secret")
	_, err = r.ResolveNames(context.Background(), scope, []string{"bad"})
	failure(t, err, "env_missing", "MISSING", "env:")
	if !strings.Contains(err.Error(), "(bad)") {
		t.Fatal(err)
	}
	if _, err = r.ResolveNames(context.Background(), nil, []string{"a"}); SecretCode(err) != "unknown_secret" {
		t.Fatal(err)
	}
}

func TestCommandPathIncludesHomebrew(t *testing.T) {
	dirs := filepath.SplitList(CommandPath)
	for _, want := range []string{"/usr/bin", "/bin", "/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"} {
		found := false
		for _, d := range dirs {
			found = found || d == want
		}
		if !found {
			t.Fatalf("CommandPath %q lacks %s", CommandPath, want)
		}
	}
	if dirs[0] != "/usr/bin" {
		t.Fatalf("system directories must come first: %q", CommandPath)
	}
}

func TestCommandName(t *testing.T) {
	for ref, want := range map[string]string{
		"cmd:gh auth token":                   "gh",
		"cmd:/opt/homebrew/bin/gh auth token": "/opt/homebrew/bin/gh",
		"cmd:  op read op://vault/item":       "op",
		"cmd:TOKEN=x gh auth token":           "",
		`cmd:"gh" auth token`:                 "",
		"cmd:":                                "",
		"env:GH_TOKEN":                        "",
		"keyring:token":                       "",
	} {
		if got := CommandName(ref); got != want {
			t.Fatalf("CommandName(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestCommandFound(t *testing.T) {
	if !CommandFound("sh") || !CommandFound("/bin/sh") {
		t.Fatal("sh not found on CommandPath")
	}
	if CommandFound("no-such-command-oc-test") || CommandFound("/nonexistent/oc-test") || CommandFound("bin/sh") || CommandFound("/usr/bin") {
		t.Fatal("missing, relative or directory path reported found")
	}
}

func TestCommandWarnings(t *testing.T) {
	cfg := `{"connections": [{"id": "github", "secret_refs": {"token": "cmd:missing-gh-oc-test auth --token hunter2", "app": "cmd:sh -c 'printf x'"}}],
		"scopes": {"web": {"secret_refs": {"a": "cmd:missing-b-oc-test", "b": "cmd:missing-gh-oc-test"}}}, "note": "cmd:outside-oc-test"}`
	got := CommandWarnings([]byte(cfg))
	if len(got) != 2 || !strings.Contains(got[0], "runs missing-b-oc-test, which is not found on PATH "+CommandPath) || !strings.Contains(got[1], "runs missing-gh-oc-test,") {
		t.Fatalf("%q", got)
	}
	for _, w := range got {
		if strings.Contains(w, "hunter2") || strings.Contains(w, "outside-oc-test") {
			t.Fatalf("warning leaks arguments or reads outside secret_refs: %q", w)
		}
	}
	if got := CommandWarnings([]byte(`{"connections": [{"secret_refs": {"token": "cmd:sh -c x"}}]}`)); len(got) != 0 {
		t.Fatalf("found command warned: %q", got)
	}
}

// TestUnenforcedWarnings: config check warns about each admission_limit the
// config sets, since nothing enforces it yet (#39).
func TestUnenforcedWarnings(t *testing.T) {
	cfg := `{"settings": {"concurrency": 2, "admission_limit": 3}, "scopes": [{"id": "web", "limits": {"admission_limit": 1}}, {"id": "api", "limits": {"concurrency": 1}}]}`
	got := UnenforcedWarnings([]byte(cfg))
	if len(got) != 2 || !strings.HasPrefix(got[0], "settings.admission_limit has no effect") || !strings.HasPrefix(got[1], "scope web limits.admission_limit has no effect") {
		t.Fatalf("%q", got)
	}
	if got := Warnings([]byte(`{"settings": {"concurrency": 2}, "scopes": [{"id": "api", "limits": {"concurrency": 1}}]}`)); len(got) != 0 {
		t.Fatalf("config without admission_limit warned: %q", got)
	}
}

// TestRemovedWarnings: config check warns about a binding that still sets the
// removed handoff experiment's handoff_share.
func TestRemovedWarnings(t *testing.T) {
	cfg := `{"scopes": [{"id": "eng", "bindings": [{"name": "agent", "config": {"handoff_share": 0}}, {"name": "other", "config": {"cold_turns": "resume"}}]}]}`
	got := Warnings([]byte(cfg))
	if len(got) != 1 || !strings.HasPrefix(got[0], "scope eng binding agent config.handoff_share is ignored") {
		t.Fatalf("%q", got)
	}
}

// TestNoPlaybookWarning: config check warns about a claude-code binding that
// names no playbook (it relies on the work-issue default a later release
// drops), and not about one that names one, "-", another executor's or a
// tombstone.
func TestNoPlaybookWarning(t *testing.T) {
	cfg := `{"scopes": [{"id": "eng", "bindings": [
		{"name": "agent", "executor": "claude-code", "config": {"branch": "agent/{number}"}},
		{"name": "named", "executor": "claude-code", "config": {"playbook": "work-issue"}},
		{"name": "none", "executor": "claude-code", "config": {"playbook": "-"}},
		{"name": "run", "executor": "shell", "config": {"command": "true"}},
		{"name": "gone", "disabled": true}]}]}`
	got := Warnings([]byte(cfg))
	if len(got) != 1 || !strings.HasPrefix(got[0], "scope eng binding agent names no playbook") {
		t.Fatalf("%q", got)
	}
}
