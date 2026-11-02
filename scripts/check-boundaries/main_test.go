package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func project(path string, dependencies ...string) pkg {
	p := pkg{ImportPath: module + path}
	for _, dependency := range dependencies {
		p.Imports = append(p.Imports, module+dependency)
	}
	return p
}

func TestDependencyClosures(t *testing.T) {
	for _, test := range []struct {
		name     string
		packages []pkg
		forbid   string
	}{
		{"public_contracts", []pkg{project("domain"), project("storage", "domain"), project("extension", "domain")}, ""},
		{"os_uses_client", []pkg{project("cmd/os", "internal/cli", "client"), project("client", "domain")}, ""},
		{"extensiontest_uses_client", []pkg{project("extensiontest", "client", "protocol"), project("client", "domain")}, ""},
		{"extensiontest_cannot_reach_internal", []pkg{project("extensiontest", "internal/cli")}, "internal/cli"},
		{"os_cannot_compose_runtime", []pkg{project("cmd/os", "internal/runtime")}, "internal/runtime"},
		{"gateway_uses_gateway", []pkg{project("cmd/open-conduit-gateway", "extensions/docker/gateway"), project("extensions/docker/gateway", "extension")}, ""},
		{"gateway_cannot_compose_runtime", []pkg{project("cmd/open-conduit-gateway", "internal/runtime")}, "internal/runtime"},
		{"direct_adapter", []pkg{project("internal/runtime", "extensions/github")}, "extensions/github"},
		{"transitive_adapter", []pkg{project("internal/runtime", "extension"), project("extension", "extensions/github")}, "extensions/github"},
		{"reference_runtime_leak", []pkg{project("conformance/reference", "internal/runtime")}, "internal/runtime"},
		{"domain_payload_leak", []pkg{project("domain", "extensions/slack")}, "extensions/slack"},
		{"executor_sqlite_leak", []pkg{project("executors/shell", "internal/storage/sqlite")}, "internal/storage/sqlite"},
		{"telemetry_uses_contracts", []pkg{project("internal/telemetry", "observability"), project("observability", "domain")}, ""},
		{"telemetry_runtime_leak", []pkg{project("internal/telemetry", "internal/runtime")}, "internal/runtime"},
		// Setup is extension-owned (04-02): core CLI hosts extension.Setup
		// values injected by the entrypoint and never imports an extension.
		{"cli_hosts_setup_contract", []pkg{project("internal/cli", "extension", "client"), project("extension", "domain"), project("client", "domain")}, ""},
		{"cli_cannot_import_github", []pkg{project("internal/cli", "extensions/github")}, "extensions/github"},
		{"cli_cannot_import_slack_transitively", []pkg{project("internal/cli", "client"), project("client", "extensions/slack")}, "extensions/slack"},
		{"tui_cannot_import_extension", []pkg{project("internal/tui", "extensions/slack")}, "extensions/slack"},
		{"os_cannot_carry_tui", []pkg{project("cmd/os", "internal/cli"), project("internal/cli", "internal/tui")}, "internal/tui"},
		// Issue reporting is an ordinary extension (03-08): it uses public
		// contracts only, and no core package can reach it.
		{"introspection_uses_contracts", []pkg{project("extensions/introspection", "domain", "extension", "storage"), project("extensions/introspection/playbooks")}, ""},
		{"runtime_cannot_import_introspection", []pkg{project("internal/runtime", "extensions/introspection")}, "extensions/introspection"},
		{"management_cannot_import_introspection", []pkg{project("internal/management", "extensions/introspection")}, "extensions/introspection"},
		{"introspection_cannot_reach_github", []pkg{project("extensions/introspection", "extensions/github")}, "extensions/github"},
		// The in-process extension contract (issue #66) is public: an
		// extension implements it with public contracts only, and it
		// reaches no runtime.
		{"extensions_implement_inproc", []pkg{project("extensions/human", "extension/inproc"), project("extension/inproc", "domain", "extension", "storage", "observability", "protocol")}, ""},
		{"inproc_cannot_reach_runtime", []pkg{project("extension/inproc", "internal/runtime")}, "internal/runtime"},
		// The shared tracker kit (06-01) is public-only; only the tracker
		// extensions may use it, and it reaches no other extension.
		{"trackers_use_kit", []pkg{project("extensions/linear", "extensions/tracker", "domain"), project("extensions/jira", "extensions/tracker"), project("extensions/bitbucket", "extensions/tracker"), project("extensions/tracker", "domain", "extension", "storage")}, ""},
		{"kit_cannot_reach_github", []pkg{project("extensions/tracker", "extensions/github")}, "extensions/github"},
		{"slack_cannot_use_kit", []pkg{project("extensions/slack", "extensions/tracker")}, "extensions/tracker"},
		{"linear_cannot_reach_jira", []pkg{project("extensions/linear", "extensions/jira")}, "extensions/jira"},
		{"runtime_cannot_import_kit", []pkg{project("internal/runtime", "extensions/tracker")}, "extensions/tracker"},
		// 06-02: the v1 converter reads extension config names, never the
		// runtime, storage or composition; the CLI reaches it by injection.
		{"migrate_reads_extensions", []pkg{project("internal/migrate", "extensions/github", "extensions/tracker"), project("extensions/github", "client", "domain"), project("extensions/tracker", "domain")}, ""},
		{"migrate_cannot_reach_runtime", []pkg{project("internal/migrate", "internal/runtime")}, "internal/runtime"},
		{"migrate_cannot_reach_sqlite", []pkg{project("internal/migrate", "internal/storage/sqlite")}, "internal/storage/sqlite"},
		{"protocol_reads_public_contracts", []pkg{project("protocol", "extension", "domain")}, ""},
		{"protocol_cannot_reach_runtime", []pkg{project("protocol", "internal/runtime")}, "internal/runtime"},
		{"protocol_cannot_reach_extensions", []pkg{project("protocol", "extensions/slack")}, "extensions/slack"},
		{"extension_cannot_import_protocol", []pkg{project("extension", "protocol")}, "protocol"},
		{"protocol_sdk_reads_protocol", []pkg{project("protocol/sdk", "protocol/jsonrpc", "protocol", "extension")}, ""},
		{"protocol_example_cannot_reach_runtime", []pkg{project("protocol/examples/notes", "internal/runtime")}, "internal/runtime"},
		{"protocol_example_cannot_reach_extensions", []pkg{project("protocol/examples/runner", "executors/shell")}, "executors/shell"},
		{"extproc_adapts_runtime", []pkg{project("internal/extproc", "internal/runtime", "protocol/jsonrpc", "storage")}, ""},
		{"extproc_cannot_reach_sqlite", []pkg{project("internal/extproc", "internal/storage/sqlite")}, "internal/storage/sqlite"},
		{"extproc_cannot_reach_bootstrap", []pkg{project("internal/extproc", "internal/bootstrap")}, "internal/bootstrap"},
		{"certify_runs_host_and_checks", []pkg{project("internal/certify", "internal/extproc", "internal/storage/sqlite", "internal/artifacts", "conformance")}, ""},
		{"certify_cannot_use_reference", []pkg{project("internal/certify", "conformance/reference")}, "conformance/reference"},
		{"certify_cannot_reach_bootstrap", []pkg{project("internal/certify", "internal/bootstrap")}, "internal/bootstrap"},
		{"package_wraps_its_implementation", []pkg{project("packages/slack", "extensions/slack", "protocol/sdk")}, ""},
		{"package_cannot_reach_another", []pkg{project("packages/slack", "executors/shell")}, "executors/shell"},
		{"package_cannot_reach_host", []pkg{project("packages/shell", "internal/extproc")}, "internal/extproc"},
		{"bootstrap_cannot_use_reference", []pkg{project("internal/bootstrap", "conformance/reference")}, "conformance/reference"},
		{"cli_cannot_import_migrate", []pkg{project("internal/cli", "internal/migrate")}, "internal/migrate"},
		{"backup_runtime_leak", []pkg{project("internal/backup", "internal/runtime")}, "internal/runtime"},
		{"unknown_package_leak", []pkg{project("newpackage", "domain")}, "domain"},
		// Setup is an installed package (#243): no package imports it, from
		// its own module or a copy back under extensions/setup.
		{"setup_package_never_imported", []pkg{{ImportPath: module + "internal/bootstrap", Imports: []string{setupPackage}}}, "setup extension"},
		{"setup_package_never_imported_transitively", []pkg{project("integration", "internal/bootstrap"), {ImportPath: module + "internal/bootstrap", Imports: []string{setupPackage + "/cmd/setup-extension"}}}, "setup extension"},
		{"setup_never_compiled_back_in", []pkg{project("internal/bootstrap", "extensions/setup")}, "setup extension"},
		{"external_bridge", []pkg{project("internal/runtime"), {ImportPath: "external.test/helper", Imports: []string{module + "extensions/git"}}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "external_bridge" {
				test.packages[0].Imports = []string{"external.test/helper"}
				test.forbid = "extensions/git"
			}
			failures := violations(test.packages)
			if test.forbid == "" {
				if len(failures) > 0 {
					t.Fatal(failures)
				}
			} else if !strings.Contains(strings.Join(failures, "\n"), test.forbid) {
				t.Fatalf("weakened graph not rejected: %v", failures)
			}
		})
	}
}

func TestStorageHarnessClosure(t *testing.T) {
	variant := project("internal/storage/sqlite_test [" + module + "internal/storage/sqlite.test]")
	variant.ForTest = module + "internal/storage/sqlite"
	variant.Imports = []string{module + "conformance"}
	graph := []pkg{variant, project("conformance", "domain", "storage", "extension", "observability")}
	if failures := violations(graph); len(failures) != 0 {
		t.Fatal(failures)
	}
	production := project("internal/storage/sqlite", "extension")
	if failures := violations([]pkg{production}); len(failures) == 0 {
		t.Fatal("test harness allowance widened production imports")
	}
}

func TestRuntimeTestVariantBoundaries(t *testing.T) {
	base := project("internal/runtime", "domain")
	variant := project("internal/runtime_test [" + module + "internal/runtime.test]")
	variant.ForTest = module + "internal/runtime"
	variant.Imports = []string{module + "internal/runtime", module + "conformance"}
	graph := []pkg{base, project("conformance", "domain"), project("domain"), variant}
	if failures := violations(graph); len(failures) > 0 {
		t.Fatal(failures)
	}
	for _, forbidden := range []string{"extensions/github", "internal/storage/sqlite", "conformance/reference"} {
		leaking := variant
		leaking.Imports = append(append([]string{}, variant.Imports...), module+forbidden)
		if failures := violations(append(graph[:len(graph)-1:len(graph)-1], leaking)); len(failures) == 0 {
			t.Fatalf("test-only %s leak passed", forbidden)
		}
	}
	// A test import may also be rewritten by Go's ImportMap to a package variant.
	helper := project("extension ["+module+"internal/runtime.test]", "extensions/slack")
	leaking := variant
	leaking.Imports = append(append([]string{}, variant.Imports...), module+"extension")
	leaking.ImportMap = map[string]string{module + "extension": helper.ImportPath}
	if failures := violations(append(graph[:len(graph)-1:len(graph)-1], leaking, helper)); len(failures) == 0 {
		t.Fatal("transitive test-variant adapter leak passed")
	}
	// Dependency tests are never part of a consumer's dependency closure.
	contractTests := project("conformance_test ["+module+"conformance.test]", "conformance/reference")
	contractTests.ForTest = module + "conformance"
	if failures := violations(append(graph, contractTests)); len(failures) > 0 {
		t.Fatal(failures)
	}
}

// TestNoPrivilegedReportingPath checks the PRD's introspection criterion at
// the source level (03-08, 05-03): no core package, contract or operator
// surface names self-improvement, issue reporting, triage, the daily sweep's
// Events, fix requests or the host watch config. They exist only as the
// introspection extension's public registrations. (Plain "sweep" is core
// vocabulary for the retention sweep.)
func TestNoPrivilegedReportingPath(t *testing.T) {
	vocabulary := regexp.MustCompile(`(?i)introspection|triage|report-issue|report_issue|self-improvement|sweep\.due|fix\.requested|fix-report|host_?watch`)
	for _, dir := range []string{"domain", "extension", "storage", "observability", "conformance", "client", "internal/runtime", "internal/config", "internal/management", "internal/storage", "internal/cli", "internal/tui", "internal/telemetry"} {
		err := filepath.WalkDir(filepath.Join("..", "..", dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if loc := vocabulary.FindIndex(data); loc != nil {
				t.Errorf("%s names %q: core has no reporting or triage path", path, data[loc[0]:loc[1]])
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestCoreImportsNoThirdPartyModule checks the core's allowlist of modules
// outside this one (#354): a database driver, a service SDK or database/sql
// in a core package is refused, while composition may import them.
func TestCoreImportsNoThirdPartyModule(t *testing.T) {
	external := func(owner string, imports ...string) pkg {
		return pkg{ImportPath: module + owner, Imports: imports}
	}
	for _, test := range []struct {
		name   string
		pkg    pkg
		forbid string
	}{
		{"runtime_uses_stdlib", external("internal/runtime", "strings", "context"), ""},
		{"runtime_sqlite_driver", external("internal/runtime", "modernc.org/sqlite"), "modernc.org/sqlite"},
		{"domain_database_sql", external("domain", "database/sql"), "database/sql"},
		{"storage_database_sql_driver", external("storage", "database/sql/driver"), "database/sql"},
		{"extension_slack_sdk", external("extension", "github.com/slack-go/slack"), "github.com/slack-go/slack"},
		{"protocol_sdk_third_party", external("protocol/sdk", "gopkg.in/yaml.v3"), "gopkg.in/yaml.v3"},
		{"inproc_third_party", external("extension/inproc", "go.opentelemetry.io/otel"), "go.opentelemetry.io/otel"},
		{"bootstrap_composes_sqlite", external("internal/bootstrap", "modernc.org/sqlite", "database/sql"), ""},
		{"sqlite_store_uses_driver", external("internal/storage/sqlite", "modernc.org/sqlite", "database/sql"), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			failures := strings.Join(violations([]pkg{test.pkg}), "\n")
			if test.forbid == "" {
				if failures != "" {
					t.Fatal(failures)
				}
			} else if !strings.Contains(failures, test.forbid) {
				t.Fatalf("third-party import not rejected: %q", failures)
			}
		})
	}
	// A transitive import through a dependency of this module is caught too.
	graph := []pkg{project("internal/runtime", "domain"), {ImportPath: module + "domain", Imports: []string{"modernc.org/sqlite"}}}
	if failures := strings.Join(violations(graph), "\n"); !strings.Contains(failures, "internal/runtime: third-party module modernc.org/sqlite") {
		t.Fatalf("transitive third-party import not rejected: %q", failures)
	}
}

func TestWords(t *testing.T) {
	for in, want := range map[string]string{
		"GitHubIssue":    "git hub issue",
		"pull_request":   "pullrequest",
		"PullRequests":   "pullrequests",
		"claude-code":    "claude code",
		"HTTPServer":     "http server",
		"Preview":        "preview",
		"slack.events.x": "slack events x",
	} {
		if got := strings.Join(words(in), " "); got != want {
			t.Errorf("words(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCoreVocabulary is the vocabulary scan's negative control: service,
// executor and workflow words in identifiers and strings are caught, and
// comments, imports, the module's own path and the toolbelt's issued
// tokens are not.
func TestCoreVocabulary(t *testing.T) {
	src := `package x

import (
	"github.com/slack-go/slack"
	gh "github.com/google/go-github"
)

import "github.com/open-conduit/open-conduit/domain"

// A GitHub issue under review is the github extension's business.
/* claude, slack */
type TokenIssuer interface{ Issue() }

const Preview = "github.com/open-conduit/open-conduit/x"

var GitHubIssue = 1
var kind = "pull_request"
var executor = "claude-code"
var states = []string{"review"}
var Reviewer = ` + "`slack`" + `
`
	found, err := tokenWords("x.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range found {
		parts := strings.SplitN(f, "\x00", 3)
		got = append(got, parts[1]+"="+parts[2])
	}
	want := []string{"github=GitHubIssue", "pullrequest=\"pull_request\"", "claude=\"claude-code\"", "review=\"review\"", "reviewer=Reviewer", "slack=`slack`"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("found %q\nwant  %q", got, want)
	}

	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/runtime/routes.go", "package runtime\n\nvar source = \"github\"\n")
	write("internal/runtime/routes_test.go", "package runtime\n\nvar fixture = \"slack\"\n")
	write("domain/turn.go", "package domain\n\nvar states = []string{\"review\"}\n")
	write("protocol/sdk/sdk.go", "package sdk\n\n// Slack and GitHub packages use this.\nfunc Serve() {}\n")
	failures, err := vocabularyViolations(root, map[string][]string{"domain/turn.go": {"review"}, "extension/gone.go": {"slack"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(failures, "\n")
	for _, want := range []string{`internal/runtime/routes.go:3:14: the core names "github"`, `extension/gone.go: no longer names "slack"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if len(failures) != 2 {
		t.Errorf("want exactly the runtime string and the stale exception, got\n%s", joined)
	}
}

// TestCompositionImports is the negative control for the composition rule:
// internal/bootstrap reaches a first-party extension or executor only from
// the wiring files, or a listed exception, and the list can only shrink.
func TestCompositionImports(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "bootstrap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"adapters.go":     `import ( "` + module + `extensions/slack"; "` + module + `executors/shell" )`,
		"inproc.go":       `import "` + module + `extensions/human"`,
		"slack.go":        `import ( "` + module + `extensions/slack"; "` + module + `extensions/github"; "` + module + `internal/runtime" )`,
		"daemon.go":       `import "` + module + `domain"`,
		"new.go":          `import x "` + module + `executors/claudecode"`,
		"github_test.go":  `import "` + module + `extensions/github"`,
		"external.go":     `import "github.com/slack-go/slack"`,
		"no_imports.go":   ``,
		"setup_scope.go":  `import "` + module + `extensionsetup"`,
		"playbook_fs.go":  `import "` + module + `extensions/slack/playbooks"`,
		"ingress_file.go": `import "` + module + `extension"`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package bootstrap\n\n"+body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	failures, err := compositionViolations(root, map[string][]string{
		"slack.go":       {"extensions/slack"},
		"daemon.go":      {"executors/shell"},
		"playbook_fs.go": {"extensions/slack/playbooks"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(failures, "\n")
	for _, want := range []string{
		"internal/bootstrap/slack.go: imports extensions/github outside the wiring files",
		"internal/bootstrap/new.go: imports executors/claudecode outside the wiring files",
		"internal/bootstrap/daemon.go: no longer imports executors/shell",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if len(failures) != 3 {
		t.Errorf("want exactly three failures, got\n%s", joined)
	}
}

// TestSourceRulesPassToday checks the repository itself passes the source
// rules, with every exception still needed.
func TestSourceRulesPassToday(t *testing.T) {
	failures, err := sourceViolations(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) > 0 {
		t.Fatal(strings.Join(failures, "\n"))
	}
}
