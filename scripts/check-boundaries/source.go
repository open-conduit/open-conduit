package main

import (
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// sourceViolations checks the source rules under the module root: the
// core's vocabulary (vocabularyViolations) and composition's extension and
// executor imports (compositionViolations).
func sourceViolations(root string) ([]string, error) {
	vocabulary, err := vocabularyViolations(root, vocabularyExceptions, verbIdentifiers)
	if err != nil {
		return nil, err
	}
	composition, err := compositionViolations(root, compositionExceptions)
	if err != nil {
		return nil, err
	}
	return append(vocabulary, composition...), nil
}

// coreDirs are the core packages' directories (see core), whose production
// source names no service, executor or workflow.
var coreDirs = []string{"domain", "extension", "storage", "observability", "protocol", "internal/runtime"}

// serviceWords are the service, executor and workflow words the core never
// names in code: an identifier or string literal holding one belongs to an
// extension or executor. Comments may name them (they explain why the core
// doesn't).
var serviceWords = map[string]bool{
	"github": true, "slack": true, "claude": true,
	"issue": true, "issues": true,
	"review": true, "reviews": true, "reviewer": true, "reviewers": true, "reviewed": true,
	"pullrequest": true, "pullrequests": true,
}

// organisation is the module's own code host path, which names no service.
const organisation = "github.com/open-conduit"

// verbIdentifiers are the identifiers that use "issue" as a verb, for the
// toolbelt's issued tokens rather than a tracker's issues, by the file that
// names them. Only these identifiers in these files are exempt, so a
// tracker's Issue type or field anywhere else is still caught. An entry
// nothing needs any more fails the check.
var verbIdentifiers = map[string][]string{
	"extension/toolbelt.go":         {"Issue"},
	"internal/runtime/toolbelt.go":  {"Issue", "IssueWithID", "issueInTx", "issueForAttempt"},
	"internal/runtime/scheduler.go": {"issueForAttempt", "issueErr"},
}

// vocabularyExceptions are the words a core file still names today, by its
// path under the module root. The list may only shrink: an entry nothing
// needs any more fails the check, so remove it with the code.
var vocabularyExceptions = map[string][]string{
	// The work-issue turn states; #343 moves them out of the core.
	"domain/turn.go": {"review"},
}

// words splits an identifier or a string into lower-case words, at
// anything but a letter or digit and at camel-case humps ("GitHubIssue" is
// git, hub, issue; "pull_request" is pull, request), and joins "pull"
// "request" into one word.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && len(cur) > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	var joined []string
	for i := 0; i < len(out); i++ {
		if out[i] == "pull" && i+1 < len(out) && (out[i+1] == "request" || out[i+1] == "requests") {
			joined = append(joined, "pull"+out[i+1])
			i++
			continue
		}
		joined = append(joined, out[i])
	}
	return joined
}

// serviceWord is the first service word in s, a word of words, or "".
// "github" is caught anywhere, since words splits "GitHub" in two.
func serviceWord(s string) string {
	// "GitHub" splits into git and hub.
	if strings.Contains(strings.ToLower(s), "github") {
		return "github"
	}
	for _, w := range words(s) {
		if serviceWords[w] {
			return w
		}
	}
	return ""
}

// tokenWords returns the service words in Go source, outside comments and
// import declarations, each with its position. The module's own import
// path is not a service word.
// Identifiers in verbs are not scanned.
func tokenWords(path string, src []byte, verbs []string) ([]string, error) {
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(src))
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(file, src, func(pos token.Position, msg string) { errs.Add(pos, msg) }, 0)
	var out []string
	importing, inParens := false, false
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		switch {
		case tok == token.IMPORT:
			importing = true
			continue
		case importing && tok == token.LPAREN:
			inParens = true
			continue
		case importing && tok == token.RPAREN:
			importing, inParens = false, false
			continue
		case importing && tok == token.SEMICOLON && !inParens:
			importing = false
			continue
		case importing:
			continue
		}
		var w string
		switch tok {
		case token.IDENT:
			if !contains(verbs, lit) {
				w = serviceWord(lit)
			}
		case token.STRING:
			text, err := strconv.Unquote(lit)
			if err != nil {
				text = lit
			}
			w = serviceWord(strings.ReplaceAll(text, organisation, ""))
		}
		if w != "" {
			out = append(out, fmt.Sprintf("%s\x00%s\x00%s", fset.Position(pos), w, lit))
		}
	}
	if len(errs) > 0 {
		return nil, errs.Err()
	}
	return out, nil
}

// vocabularyViolations scans the core's production source (coreDirs, not
// its tests) for service words in identifiers and string literals, but for
// the exceptions and the verb identifiers, each of which must still be
// needed.
func vocabularyViolations(root string, exceptions, verbs map[string][]string) ([]string, error) {
	used := map[string]bool{}
	var errors []string
	for _, dir := range coreDirs {
		if _, err := os.Stat(filepath.Join(root, dir)); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			found, err := tokenWords(rel, src, verbs[rel])
			if err != nil {
				return err
			}
			if len(verbs[rel]) > 0 {
				names, err := identifiers(rel, src)
				if err != nil {
					return err
				}
				for _, v := range verbs[rel] {
					if names[v] {
						used["verb\x00"+rel+"\x00"+v] = true
					}
				}
			}
			for _, f := range found {
				parts := strings.SplitN(f, "\x00", 3)
				if contains(exceptions[rel], parts[1]) {
					used[rel+"\x00"+parts[1]] = true
					continue
				}
				errors = append(errors, fmt.Sprintf("%s: the core names %q (in %s): services, executors and workflows belong to extensions", parts[0], parts[1], parts[2]))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for file, ws := range exceptions {
		for _, w := range ws {
			if !used[file+"\x00"+w] {
				errors = append(errors, fmt.Sprintf("%s: no longer names %q; remove it from vocabularyExceptions", file, w))
			}
		}
	}
	for file, vs := range verbs {
		for _, v := range vs {
			if !used["verb\x00"+file+"\x00"+v] {
				errors = append(errors, fmt.Sprintf("%s: no longer names %s; remove it from verbIdentifiers", file, v))
			}
		}
	}
	sort.Strings(errors)
	return errors, nil
}

// identifiers are the identifiers Go source names.
func identifiers(path string, src []byte) (map[string]bool, error) {
	fset := token.NewFileSet()
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(fset.AddFile(path, -1, len(src)), src, func(pos token.Position, msg string) { errs.Add(pos, msg) }, 0)
	out := map[string]bool{}
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT {
			out[lit] = true
		}
	}
	if len(errs) > 0 {
		return nil, errs.Err()
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// wiringFiles are the composition files that may import any first-party
// extension or executor: the in-process registrations (firstParty) and the
// catalogue adapters.
var wiringFiles = map[string]bool{"inproc.go": true, "adapters.go": true}

// compositionExceptions are the other internal/bootstrap files that import
// a first-party extension or executor today, and what they import: bespoke
// per-integration wiring that predates the public contracts (#357 moves it
// onto them). The list may only shrink: a file may not import anything new,
// and an entry nothing needs any more fails the check, so remove it with
// the import.
var compositionExceptions = map[string][]string{
	"capacity.go":   {"extensions/github", "extensions/schedule"},
	"claudecode.go": {"executors/claudecode", "extensions/github"},
	"container.go":  {"extensions/docker"},
	"daemon.go":     {"executors/shell", "extensions/bitbucket", "extensions/git", "extensions/github", "extensions/schedule", "extensions/slack", "extensions/tracker"},
	"github.go":     {"extensions/github"},
	"loop.go":       {"extensions/schedule"},
	"operator.go":   {"executors/claudecode", "executors/shell", "extensions/github", "extensions/github/playbooks", "extensions/jira", "extensions/linear", "extensions/schedule", "extensions/slack"},
	"probes.go":     {"executors/claudecode", "extensions/github"},
	"resources.go":  {"extensions/git"},
	"routes.go":     {"extensions/github", "extensions/schedule", "extensions/slack"},
	"sandbox.go":    {"extensions/srt"},
	"schedule.go":   {"extensions/github", "extensions/schedule"},
	"slack.go":      {"extensions/slack"},
	"state.go":      {"extensions/posthog"},
	"tools.go":      {"executors/claudecode", "extensions/github", "extensions/github/playbooks", "extensions/slack", "extensions/slack/playbooks", "extensions/tracker/playbooks"},
	"trackers.go":   {"extensions/bitbucket", "extensions/jira", "extensions/linear", "extensions/tracker"},
	"webhooks.go":   {"extensions/github", "extensions/schedule", "extensions/slack", "extensions/tunnel"},
}

// compositionViolations checks internal/bootstrap's production files import
// a first-party extension or executor only from wiringFiles, or as the
// exceptions allow, each of which must still be needed.
func compositionViolations(root string, exceptions map[string][]string) ([]string, error) {
	dir := filepath.Join(root, "internal", "bootstrap")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	var errors []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			dependency := strings.TrimPrefix(path, module)
			if dependency == path || !(under(dependency, "extensions") || under(dependency, "executors")) || wiringFiles[name] {
				continue
			}
			if contains(exceptions[name], dependency) {
				used[name+"\x00"+dependency] = true
				continue
			}
			errors = append(errors, fmt.Sprintf("internal/bootstrap/%s: imports %s outside the wiring files (%s); register it through the public contracts instead", name, dependency, strings.Join(sortedKeys(wiringFiles), ", ")))
		}
	}
	for file, deps := range exceptions {
		for _, d := range deps {
			if !used[file+"\x00"+d] {
				errors = append(errors, fmt.Sprintf("internal/bootstrap/%s: no longer imports %s; remove it from compositionExceptions", file, d))
			}
		}
	}
	sort.Strings(errors)
	return errors, nil
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
