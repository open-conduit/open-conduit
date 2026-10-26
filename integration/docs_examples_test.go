package integration_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// userDocs are the user-facing documents whose examples must work as
// written: complete example configs validate offline, every open-conduit
// command they show exists with the flags it uses, and every make target
// they show is in the Makefile.
var userDocs = []string{"../README.md", "../docs/README.md", "../CONTRIBUTING.md", "../SECURITY.md"}

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
// `open-conduit config check`.
func TestDocsExampleConfigsValidate(t *testing.T) {
	total := 0
	for _, path := range userDocs {
		for i, block := range fencedBlocks(t, path, "json") {
			var probe map[string]json.RawMessage
			if json.Unmarshal([]byte(block), &probe) != nil || probe["version"] == nil {
				continue
			}
			total++
			diags, err := bootstrap.ValidateConfig([]byte(block))
			if err != nil || len(diags) != 0 {
				t.Errorf("%s: json block %d is not a valid config: %v %v", filepath.Base(path), i+1, diags, err)
			}
		}
	}
	if total < 4 {
		t.Fatalf("found %d example configs in the user docs, want at least one per workflow (4)", total)
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
