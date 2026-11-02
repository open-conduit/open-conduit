package integration_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sleepAllowance is a file's permitted time.Sleep references and why.
type sleepAllowance struct {
	count  int
	reason string
}

// sleepAllowlist is every time.Sleep the integration, conformance and runtime suites
// still hold. It may only shrink: a new sleep fails TestNoSleep, and so does
// an entry that allows more than its file has. Wait on a condition with
// eventually, or on the daemon's loop barrier with drill.settle, instead.
var sleepAllowlist = map[string]sleepAllowance{
	"integration/daemon_test.go":      {1, "the expired-lease kill drill restarts only after the dead daemon's lease has really expired; daemon leases run on the wall clock, which a drill cannot advance"},
	"integration/sandbox_test.go":     {1, "the sandboxed command runs inside srt with no signal that it has settled before a control is sent"},
	"internal/runtime/timers_test.go": {1, "the crash helper parks after SIGKILLing itself, so the Go runtime cannot report a deadlock first; no test waits on it"},
}

// sleepRefs counts the references to the standard library's time.Sleep in
// one Go source file, under whatever name the file imports time as.
func sleepRefs(name string, src []byte) (int, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
	if err != nil {
		return 0, err
	}
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path != "time" {
			continue
		}
		switch {
		case imp.Name == nil:
			names["time"] = true
		case imp.Name.Name == ".":
			return 0, fmt.Errorf("%s dot-imports time", name)
		case imp.Name.Name != "_":
			names[imp.Name.Name] = true
		}
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sleep" {
			if x, ok := sel.X.(*ast.Ident); ok && names[x.Name] {
				n++
			}
		}
		return true
	})
	return n, nil
}

// sleepProblems compares each file's time.Sleep references with the
// allowlist: more than allowed is a new sleep, fewer leaves a stale entry.
func sleepProblems(counts map[string]int, allow map[string]sleepAllowance) []string {
	var out []string
	for file, n := range counts {
		if a := allow[file]; n > a.count {
			out = append(out, fmt.Sprintf("%s: %d time.Sleep call(s), %d allowed; wait on a condition (eventually) or the loop barrier (drill.settle) instead", file, n, a.count))
		}
	}
	for file, a := range allow {
		if a.reason == "" {
			out = append(out, file+": an allowlist entry needs a reason")
		}
		if n := counts[file]; n < a.count {
			out = append(out, fmt.Sprintf("%s: %d time.Sleep call(s) but %d allowed; lower the allowlist entry", file, n, a.count))
		}
	}
	sort.Strings(out)
	return out
}

// TestNoSleep: tests never wait on real time, so no Go file in integration/,
// conformance/ or internal/runtime/ calls time.Sleep beyond the shrinking allowlist.
func TestNoSleep(t *testing.T) {
	counts := map[string]int{}
	for _, dir := range []string{"../integration", "../conformance", "../internal/runtime"} {
		err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			n, err := sleepRefs(path, src)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel("..", path)
			if n > 0 {
				counts[filepath.ToSlash(rel)] = n
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range sleepProblems(counts, sleepAllowlist) {
		t.Error(p)
	}
}

// TestNoSleepRejectsNewSleep is TestNoSleep's negative control: the checker
// finds a sleep under an aliased import and as a function value, ignores a
// local Sleep, and refuses a new or extra sleep and a stale entry.
func TestNoSleepRejectsNewSleep(t *testing.T) {
	sample := `package x

import (
	clock "time"
	"testing"
)

type fake struct{}

func (fake) Sleep(clock.Duration) {}

func TestX(t *testing.T) {
	clock.Sleep(clock.Second)
	pause := clock.Sleep
	pause(1)
	fake{}.Sleep(1)
}
`
	n, err := sleepRefs("sample_test.go", []byte(sample))
	if err != nil || n != 2 {
		t.Fatalf("sample: %d time.Sleep references, %v; want 2", n, err)
	}
	if n, err = sleepRefs("clean_test.go", []byte("package x\n\nimport \"time\"\n\nvar d = time.Second\n")); err != nil || n != 0 {
		t.Fatalf("clean sample: %d, %v", n, err)
	}
	if _, err = sleepRefs("dot_test.go", []byte("package x\n\nimport . \"time\"\n\nvar d = Second\n")); err == nil {
		t.Fatal("a dot import of time was accepted")
	}
	allow := map[string]sleepAllowance{"integration/old_test.go": {1, "a reason"}}
	for name, counts := range map[string]map[string]int{
		"a new file":     {"integration/old_test.go": 1, "integration/new_test.go": 1},
		"an extra sleep": {"integration/old_test.go": 2},
		"a stale entry":  {},
	} {
		if len(sleepProblems(counts, allow)) == 0 {
			t.Errorf("%s was accepted", name)
		}
	}
	if p := sleepProblems(map[string]int{"integration/old_test.go": 1}, allow); len(p) != 0 {
		t.Errorf("the allowed sleep was refused: %v", p)
	}
	if p := sleepProblems(nil, map[string]sleepAllowance{"integration/old_test.go": {0, ""}}); len(p) == 0 {
		t.Error("an entry without a reason was accepted")
	}
}
