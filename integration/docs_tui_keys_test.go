package integration_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/tui"
)

var updateDocs = flag.Bool("update-docs", false, "rewrite generated sections of the docs")

const (
	operatingGuide = "../site/src/content/docs/guides/operating.md"
	tuiKeysStart   = "<!-- generated: tui keys (go test ./integration -run TestDocsTUIKeys -update-docs) -->\n"
	tuiKeysEnd     = "<!-- end generated: tui keys -->\n"
)

// The operating guide's key table is the TUI's own key table, the one its
// footers render from, so a key added to the view shows up in the guide.
func TestDocsTUIKeys(t *testing.T) {
	data, err := os.ReadFile(operatingGuide)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	got, problem := replaceSection(doc, tuiKeysStart, tuiKeysEnd, "\n"+tui.KeyTable()+"\n")
	if problem != "" {
		t.Fatal(problem)
	}
	if got == doc {
		return
	}
	if *updateDocs {
		if err := os.WriteFile(operatingGuide, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("%s: the TUI key table is out of date; run go test ./integration -run TestDocsTUIKeys -update-docs. Want:\n%s", operatingGuide, tui.KeyTable())
}

// The check fails on a guide whose table lacks a key the view has.
func TestDocsTUIKeysCatchesDrift(t *testing.T) {
	stale := tuiKeysStart + "| Key | Does |\n| --- | --- |\n| `R` | run or retry |\n" + tuiKeysEnd
	got, err := replaceSection(stale, tuiKeysStart, tuiKeysEnd, tui.KeyTable())
	if err != "" || got == stale {
		t.Fatalf("a stale table passed: %q", err)
	}
	if _, err := replaceSection("no markers", tuiKeysStart, tuiKeysEnd, ""); err == "" {
		t.Fatal("a guide without the markers passed")
	}
}

// replaceSection swaps the text between the start and end markers.
func replaceSection(doc, start, end, body string) (string, string) {
	i := strings.Index(doc, start)
	j := strings.Index(doc, end)
	if i < 0 || j < i {
		return "", "missing the generated section markers " + strings.TrimSpace(start) + " … " + strings.TrimSpace(end)
	}
	return doc[:i+len(start)] + body + doc[j:], ""
}
