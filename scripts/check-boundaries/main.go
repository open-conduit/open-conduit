// Command check-boundaries enforces B01 against Go's resolved production and test
// dependency graph. It uses no runtime implementations or third-party packages.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const module = "github.com/open-conduit/open-conduit/"

type pkg struct {
	ImportPath string
	ForTest    string
	Imports    []string
	ImportMap  map[string]string
}

func clean(path string) string { return strings.SplitN(path, " [", 2)[0] }
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// trackerKitUser names the first-party tracker extensions allowed to use the
// shared tracker kit (extensions/tracker): Linear and Jira, and Bitbucket,
// whose pull requests correlate with a tracker's issue keys.
func trackerKitUser(family string) bool {
	return family == "extensions/linear" || family == "extensions/jira" || family == "extensions/bitbucket"
}

func public(path string) bool {
	return path == "domain" || path == "extension" || path == "storage" || path == "observability" || path == "protocol" || path == "extension/inproc"
}

// allowed includes the entire permitted project dependency closure, not just
// direct edges. Tests may use the public conformance harness to verify real
// implementations; runtime tests may never substitute conformance/reference.
func allowed(owner, dependency string, test bool) bool {
	if owner == dependency {
		return true
	}
	if test && !public(owner) && (dependency == "conformance" || public(dependency)) {
		return true
	}
	switch {
	case owner == "domain":
		return false
	case owner == "extension", owner == "storage", owner == "observability":
		return dependency == "domain"
	case owner == "extension/inproc":
		// The in-process extension contract (issue #66) is stated in the
		// public contracts and the protocol's configure shape only, so a
		// first-party extension reaches nothing an installed package could not.
		return public(dependency)
	case owner == "protocol":
		// The extracted subprocess protocol (07-01) describes the public
		// contracts on the wire; it never reaches an implementation.
		return public(dependency)
	case strings.HasPrefix(owner, "protocol/"):
		// The wire transport, the extension-side SDK and the example
		// external extension and executor (07-02) use only the public
		// contracts and the protocol, exactly as a third party would.
		return public(dependency) || under(dependency, "protocol")
	case under(owner, "internal/extproc"):
		// The host runtime for extension processes (07-02) adapts the
		// protocol onto the public contracts and the runtime's trusted
		// acceptance, causal and timer surfaces; never SQLite, bootstrap
		// or another extension.
		return public(dependency) || under(dependency, "protocol") || under(dependency, "internal/runtime") || under(dependency, "internal/extproc")
	case under(owner, "internal/certify"):
		// The extension conformance runner (07-03) starts a package exactly
		// as the daemon does: the host runtime behind real SQLite and
		// artifact stores, judged by the public conformance checks (never
		// the reference doubles, bootstrap or an extension).
		return public(dependency) || under(dependency, "protocol") || under(dependency, "internal/extproc") || under(dependency, "internal/runtime") || under(dependency, "internal/storage/sqlite") || under(dependency, "internal/artifacts") || under(dependency, "internal/certify") || dependency == "conformance"
	case under(owner, "packages"):
		// A first-party subprocess package (07-03) serves exactly one
		// in-process implementation over the protocol, as a third party
		// would: packages/<name> wraps extensions/<name> or
		// executors/<name>, never another implementation or the host.
		parts := strings.Split(owner, "/")
		name := parts[min(len(parts)-1, 1)]
		return public(dependency) || under(dependency, "protocol") || under(dependency, "packages/"+name) || under(dependency, "extensions/"+name) || under(dependency, "executors/"+name)
	case under(owner, "conformance"):
		return public(dependency) || under(dependency, "conformance")
	case under(owner, "internal/runtime"):
		return public(dependency) || under(dependency, "internal/runtime")
	case under(owner, "internal/storage/sqlite"):
		return dependency == "domain" || dependency == "storage" || under(dependency, "internal/storage/sqlite")
	case under(owner, "internal/artifacts"):
		return dependency == "domain" || dependency == "storage" || under(dependency, "internal/artifacts")
	case under(owner, "internal/backup"):
		return dependency == "domain" || dependency == "storage" || under(dependency, "internal/backup") || under(dependency, "internal/storage/sqlite") || under(dependency, "internal/artifacts")
	case under(owner, "internal/telemetry"):
		// Lifecycle export pipeline: public contracts plus the OpenTelemetry API.
		return public(dependency) || under(dependency, "internal/telemetry")
	case under(owner, "internal/config"):
		return dependency == "domain" || dependency == "extension" || under(dependency, "internal/config")
	case under(owner, "client"):
		return public(dependency) || under(dependency, "client")
	case under(owner, "internal/migrate"):
		// Explicit Open Supervisor conversion (06-02) writes extension and
		// executor config: it may read their exported names and setup shapes,
		// never runtime, SQLite, management or bootstrap.
		return public(dependency) || under(dependency, "internal/migrate") || under(dependency, "client") || under(dependency, "extensions") || under(dependency, "executors")
	case under(owner, "internal/management"):
		return public(dependency) || under(dependency, "internal/management") || under(dependency, "internal/runtime") || under(dependency, "internal/config")
	case under(owner, "internal/cli"), under(owner, "internal/tui"):
		return public(dependency) || under(dependency, "client") || under(dependency, "internal/cli") || under(dependency, "internal/tui")
	case under(owner, "internal/web"):
		// The browser view (like the TUI) is a management client only.
		return public(dependency) || under(dependency, "client") || under(dependency, "internal/cli") || under(dependency, "internal/web")
	case under(owner, "extensions/tracker"):
		// The shared tracker kit (06-01) is public contracts only.
		return public(dependency) || under(dependency, "extensions/tracker")
	case under(owner, "extensions"), under(owner, "executors"):
		// An extension/executor cannot reach another implementation indirectly.
		// The tracker extensions share the public-only tracker kit.
		parts := strings.Split(owner, "/")
		family := strings.Join(parts[:min(len(parts), 2)], "/")
		if trackerKitUser(family) && under(dependency, "extensions/tracker") {
			return true
		}
		return public(dependency) || under(dependency, "client") || under(dependency, family)
	case owner == "cmd/os":
		return public(dependency) || under(dependency, "client") || under(dependency, "internal/cli")
	case under(owner, "internal/bootstrap"), owner == "cmd/open-conduit", under(owner, "integration"):
		// Composition and integration tests assemble implementations. Production
		// code must still never acquire volatile reference doubles; it reaches
		// the public conformance checks only through the certification runner
		// (07-03), which ships them as `open-conduit extension certify`.
		return test || !under(dependency, "conformance") || dependency == "conformance"
	case owner == "scripts/check-boundaries":
		return false
	default:
		// New packages require an explicit ownership decision in B01.
		return false
	}
}

func violations(packages []pkg) []string {
	graph := map[string]pkg{}
	for _, p := range packages {
		graph[p.ImportPath] = p
	}
	var errors []string
	for _, root := range packages {
		owner := root.ImportPath
		if root.ForTest != "" {
			owner = root.ForTest
		}
		owner = clean(owner)
		if !strings.HasPrefix(owner, module) || strings.HasSuffix(owner, ".test") {
			continue // Go-generated test mains are composition, not source packages.
		}
		owner = strings.TrimPrefix(owner, module)
		seen := map[string]bool{}
		var visit func(pkg, []string)
		visit = func(current pkg, chain []string) {
			for _, imported := range current.Imports {
				if mapped, ok := current.ImportMap[imported]; ok {
					imported = mapped
				}
				if seen[imported] {
					continue
				}
				seen[imported] = true
				next := append(append([]string{}, chain...), imported)
				dependency := clean(imported)
				if strings.HasPrefix(dependency, module) && !allowed(owner, strings.TrimPrefix(dependency, module), root.ForTest != "") {
					errors = append(errors, fmt.Sprintf("%s: forbidden dependency via %s", root.ImportPath, strings.Join(next, " -> ")))
					continue
				}
				if child, ok := graph[imported]; ok {
					visit(child, next)
				}
			}
		}
		visit(root, []string{root.ImportPath})
	}
	sort.Strings(errors)
	return errors
}

func main() {
	command := exec.Command("go", "list", "-deps", "-test", "-json", "./...")
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "boundary check: go list failed:", err)
		os.Exit(1)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	var packages []pkg
	for {
		var p pkg
		if err := decoder.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "boundary check: invalid go list output:", err)
			os.Exit(1)
		}
		packages = append(packages, p)
	}
	if failures := violations(packages); len(failures) > 0 {
		for _, failure := range failures {
			fmt.Fprintln(os.Stderr, failure)
		}
		os.Exit(1)
	}
	fmt.Println("B01 boundaries: production and test dependency closures pass")
}
