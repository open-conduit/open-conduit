package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// userDocs are the user-facing documents whose examples must work as
// written: complete example configs validate offline, every open-conduit
// command they show exists with the flags it uses, and every make target
// they show is in the Makefile.
var userDocs = append([]string{"../README.md", "../docs/README.md", "../CONTRIBUTING.md", "../SECURITY.md"}, siteDocs()...)

// siteDocs are the Markdown pages of the published docs site.
func siteDocs() []string {
	var out []string
	_ = filepath.WalkDir("../site/src/content/docs", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".mdx")) {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// fencedBlocks returns the bodies of the ```lang blocks in a Markdown file.
func fencedBlocks(t *testing.T, path, lang string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var cur *strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case cur == nil && trimmed == "```"+lang:
			cur = &strings.Builder{}
		case cur != nil && trimmed == "```":
			out = append(out, cur.String())
			cur = nil
		case cur != nil:
			cur.WriteString(line + "\n")
		}
	}
	if cur != nil {
		t.Fatalf("%s: unterminated code block", path)
	}
	return out
}

// Every complete config document (a JSON object with "version") in the
// user-facing docs passes the same offline validation as
// `open-conduit config check`, without a warning: an example that prints one
// teaches a reader something deprecated or ineffective. The warnings that
// depend on the machine (a cmd: secret's program not being installed, an
// included folder being missing) are left out.
// releaseLayout checks configuration offline, for the rest of the test, as
// the release's binary would: with the packages it bundles beside it,
// which the built open-conduit serves (#428). Packages are found there
// and run by that binary rather than this test's own.
func releaseLayout(t *testing.T) {
	t.Helper()
	bin := daemonBinary(t)
	restore := bootstrap.UseBundledRoot(filepath.Join(filepath.Dir(bin), extproc.Dir))
	prev := extproc.HostExecutable
	extproc.HostExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() {
		restore()
		extproc.HostExecutable = prev
	})
}

func TestDocsExampleConfigsValidate(t *testing.T) {
	releaseLayout(t)
	total := 0
	for _, path := range userDocs {
		for i, block := range fencedBlocks(t, path, "json") {
			var probe map[string]json.RawMessage
			if json.Unmarshal([]byte(block), &probe) != nil || probe["version"] == nil {
				continue
			}
			if probe["settings"] == nil && probe["connections"] == nil {
				validateFolderExample(t, path, i, block)
				continue
			}
			total++
			diags, modes, err := bootstrap.CheckConfigAt("", []byte(block))
			if err != nil || len(diags) != 0 {
				t.Errorf("%s: json block %d is not a valid config: %v %v", filepath.Base(path), i+1, diags, err)
			}
			if w := bootstrap.ConfigWarnings("", []byte(block), modes, false); len(w) != 0 {
				t.Errorf("%s: json block %d warns when checked: %v", filepath.Base(path), i+1, w)
			}
		}
	}
	if total < 4 {
		t.Fatalf("found %d example configs in the user docs, want at least one per workflow (4)", total)
	}
}

// validateFolderExample checks an included folder's config (a document with
// only version and scopes) composed beneath a parent scope.
func validateFolderExample(t *testing.T, path string, i int, block string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "folder", ".open-conduit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "folder", ".open-conduit", "config.json"), []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := `{"version": 1, "settings": {}, "connections": [], "includes": [{"path": "folder", "parent": "parent"}],
		"scopes": [{"id": "parent", "name": "Parent", "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome"], "read_scopes": []}}]}`
	diags, err := bootstrap.ValidateConfigAt(filepath.Join(dir, "config.json"), []byte(parent))
	if err != nil || len(diags) != 0 {
		t.Errorf("%s: json block %d is not a valid folder config: %v %v", filepath.Base(path), i+1, diags, err)
	}
}

var (
	docCommand     = regexp.MustCompile(`^open-conduit\s+(.*)$`)
	docMake        = regexp.MustCompile(`^(?:[A-Z][A-Z0-9_]*=\S+\s+)*make\s+([a-z][a-z0-9-]*)`)
	makeTarget     = regexp.MustCompile(`(?m)^([a-z][a-z0-9-]*):`)
	docFlag        = regexp.MustCompile(`(?:^|\s)(--[a-z][a-z0-9-]*)`)
	subcommandLike = regexp.MustCompile(`^[a-z]+$`)
)

// helpText is what `open-conduit --help` and `open-conduit init --help`
// print, with the registered setups.
func helpText(t *testing.T) string {
	t.Helper()
	env := cli.Env{Getenv: func(string) string { return "" }, Cwd: t.TempDir(), Setups: bootstrap.Setups()}
	var all bytes.Buffer
	for _, args := range [][]string{{"--help"}, {"init", "--help"}, {"extension", "--help"}} {
		var out, errOut bytes.Buffer
		cli.RunOperator("test", "docs", args, env, &out, &errOut)
		all.Write(out.Bytes())
		all.Write(errOut.Bytes())
	}
	return all.String()
}

// Every open-conduit command in the user docs' shell blocks names a
// command (and subcommand) the help lists, with flags it documents.
func TestDocsCommandsExist(t *testing.T) {
	help := helpText(t)
	words := map[string]bool{}
	for _, f := range strings.FieldsFunc(help, func(r rune) bool { return strings.ContainsRune(" \t\n[]|=(),", r) }) {
		words[f] = true
	}
	checked := 0
	for _, path := range userDocs {
		for _, block := range fencedBlocks(t, path, "sh") {
			for _, line := range strings.Split(block, "\n") {
				line = strings.TrimSpace(line)
				if i := strings.Index(line, "#"); i >= 0 {
					line = strings.TrimSpace(line[:i])
				}
				m := docCommand.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				checked++
				fields := strings.Fields(m[1])
				if len(fields) == 0 || !words[fields[0]] {
					t.Errorf("%s: %q: unknown command", filepath.Base(path), line)
					continue
				}
				// A lowercase second word is a subcommand or setup type
				// (config check, init github); paths, references and values
				// are not checked.
				if len(fields) > 1 && subcommandLike.MatchString(fields[1]) && !words[fields[1]] {
					t.Errorf("%s: %q: unknown subcommand %q", filepath.Base(path), line, fields[1])
				}
				for _, f := range docFlag.FindAllStringSubmatch(m[1], -1) {
					if !words[f[1]] {
						t.Errorf("%s: %q: flag %s is not in the help", filepath.Base(path), line, f[1])
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no open-conduit commands found in the user docs")
	}
}

// Every make command in the user docs' shell blocks (optionally after
// environment assignments such as GOTOOLCHAIN=go1.24.2) names a target the
// Makefile defines.
func TestDocsMakeTargetsExist(t *testing.T) {
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]bool{}
	for _, m := range makeTarget.FindAllStringSubmatch(string(makefile), -1) {
		targets[m[1]] = true
	}
	checked := 0
	for _, path := range userDocs {
		for _, block := range fencedBlocks(t, path, "sh") {
			for _, line := range strings.Split(block, "\n") {
				m := docMake.FindStringSubmatch(strings.TrimSpace(line))
				if m == nil {
					continue
				}
				checked++
				if !targets[m[1]] {
					t.Errorf("%s: %q: make target %q is not in the Makefile", filepath.Base(path), strings.TrimSpace(line), m[1])
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no make commands found in the user docs")
	}
}

// Offline doctor runs a bundled package's offline checks when the
// configuration uses it (#428): GitHub's capability and git-bot checks need
// no daemon. A package the configuration doesn't use is never started.
func TestOfflineDoctorRunsPackageChecks(t *testing.T) {
	releaseLayout(t)
	config := []byte(`{"version": 1, "settings": {}, "connections": [], "scopes": [{"id": "ops", "name": "Operations",
		"authorization": {"connections": [], "operations": ["read_context"], "read_scopes": []},
		"bindings": [{"name": "run", "executor": "shell", "requires": {"toolbelt": "cli"}, "operations": ["read_context"], "secret_names": [], "config": {"command": "true"}}],
		"routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}], "secret_refs": {}}]}`)
	names := func(config []byte) map[string]bool {
		got := map[string]bool{}
		for _, c := range bootstrap.OfflineChecks(context.Background(), filepath.Join(t.TempDir(), "config.json"), config) {
			got[c.Name] = true
		}
		return got
	}
	if got := names(config); !got["config"] || got["github-capability"] {
		t.Fatalf("offline checks of a config without GitHub: %v", got)
	}
	withGitHub := []byte(`{"version": 1, "settings": {}, "connections": [{"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "env:OC_TEST_GITHUB_TOKEN"}}],
		"scopes": [{"id": "eng", "name": "Engineering",
		"authorization": {"connections": ["github"], "operations": ["read_context"], "read_scopes": []},
		"bindings": [{"name": "run", "executor": "shell", "requires": {"toolbelt": "cli"}, "operations": ["read_context"], "secret_names": [], "config": {"command": "true"}}],
		"routes": [{"name": "issues", "handler": "github", "binding": "run", "active_policy": "queue", "config": {}}],
		"extensions": [{"name": "widgets", "type": "github", "connection": "github", "secret_names": [], "config": {"namespace": "github/github.com/octo/widgets", "scope": "eng", "repo": "octo/widgets"}}],
		"secret_refs": {}}]}`)
	if got := names(withGitHub); !got["config"] || !got["github-capability"] || !got["github-git-bot"] {
		t.Fatalf("offline checks of a config with GitHub: %v", got)
	}
	// claude-code's own check runs each claude-code binding's binary, and
	// names the bindings of one that doesn't run.
	withClaude := []byte(`{"version": 1, "settings": {}, "connections": [], "scopes": [{"id": "ops", "name": "Operations",
		"authorization": {"connections": [], "operations": ["read_context", "declare_outcome"], "read_scopes": []},
		"bindings": [{"name": "agent", "executor": "claude-code", "requires": {"toolbelt": "mcp"}, "operations": ["read_context", "declare_outcome"], "secret_names": [], "config": {"binary": "/nonexistent/claude-oc-test"}}],
		"routes": [{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": {}}], "secret_refs": {}}]}`)
	var claude runtime.Check
	for _, c := range bootstrap.OfflineChecks(context.Background(), filepath.Join(t.TempDir(), "config.json"), withClaude) {
		if c.Name == "claude-code" {
			claude = c
		}
	}
	if claude.Status != runtime.CheckFail || claude.Code != "claude_missing" || len(claude.Items) != 1 || !strings.Contains(claude.Items[0], "ops/agent") {
		t.Fatalf("claude-code check offline: %+v", claude)
	}
}

// TestModeWarnings: offline, a claude-code binding whose git resource sets a
// mode the executor's own workspace_mode overrides is warned about, asked of
// the bundled claude-code package, and the same binding asking for the
// resource's mode, or a resource naming no mode, is not.
func TestModeWarnings(t *testing.T) {
	releaseLayout(t)
	config := func(executorMode string) []byte {
		return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"review","name":"review","bindings":[{"name":"reviewer","executor":"claude-code",
			"requires":{"workspace":true},"operations":[],"secret_names":[],"config":{"playbook":"-"` + executorMode + `},
			"resource":{"provider":"git","config":{"checkout":"./checkout","mode":"fresh"}}}]}]}`)
	}
	if diags, err := bootstrap.ValidateConfigAt("", config("")); err != nil || len(diags) != 0 {
		t.Fatalf("config invalid: %v %+v", err, diags)
	}
	got := bootstrap.NewConfigCheck().ModeWarnings("", config(""))
	if len(got) != 1 || !strings.HasPrefix(got[0], `scope review binding reviewer: the git resource's mode "fresh" never applies, because executor claude-code requests mode "work"`) {
		t.Fatalf("warnings %q", got)
	}
	if got := bootstrap.NewConfigCheck().ModeWarnings("", config(`,"workspace_mode":"fresh"`)); len(got) != 0 {
		t.Fatalf("agreeing modes warned: %q", got)
	}
	// A resource that names no mode is quiet, though its default is shared.
	unset := strings.Replace(string(config("")), `,"mode":"fresh"`, "", 1)
	if got := bootstrap.NewConfigCheck().ModeWarnings("", []byte(unset)); len(got) != 0 {
		t.Fatalf("unset mode warned: %q", got)
	}
}
