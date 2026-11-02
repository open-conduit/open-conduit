package claudecode_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// domainTerms are the code-host and issue-tracking words the executor must
// not use outside its tests: it runs any kind of Work, and what a turn means
// for an issue, a PR or a deploy is the playbook's and the owning
// extension's to say. The module path is not a use.
var domainTerms = regexp.MustCompile(`(?i)\bgit\s*hub\b|\bgitlab\b|\bbitbucket\b|\bissues?\b|pull request|\bpreviews?\b|\bdeploy\w*|\bchecks?\b|\bmerged?\b|\b(?-i:PRs?)\b`)

const modulePath = "github.com/open-conduit/open-conduit"

func domainTermsIn(src string) []string {
	var out []string
	for i, line := range strings.Split(strings.ReplaceAll(src, modulePath, ""), "\n") {
		if m := domainTerms.FindString(line); m != "" {
			out = append(out, strings.TrimSpace(line)+"  ← "+m+" (line "+strconv.Itoa(i+1)+")")
		}
	}
	return out
}

// TestNoDomainTerms: no non-test source in the executor names a code host,
// an issue, a PR, a check, a preview, a deploy or a merge.
func TestNoDomainTerms(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		n++
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range domainTermsIn(string(src)) {
			t.Errorf("%s: %s", f, hit)
		}
	}
	if n == 0 {
		t.Fatal("no executor sources found")
	}
}

// TestDomainTermsNegativeControl: the scan catches each kind of term, and
// passes the module path and ordinary words that only contain one.
func TestDomainTermsNegativeControl(t *testing.T) {
	for _, src := range []string{
		`// building: you pushed and are waiting for checks or a preview.`,
		`"description": "Every PR number that belongs to this issue."`,
		`var x = "credential.https://github.com.helper"`,
		`// GitHub token`,
		`// the PRs merged`,
		`// open a pull request`,
		`// a deploy preview`,
		`// the Bitbucket or GitLab host`,
	} {
		if len(domainTermsIn(src)) == 0 {
			t.Errorf("not caught: %s", src)
		}
	}
	for _, src := range []string{
		`import "` + modulePath + `/domain"`,
		`// a prompt, a process, a preset, issued tokens`,
		`// the checkout's previewer`,
	} {
		if hits := domainTermsIn(src); len(hits) != 0 {
			t.Errorf("false positive: %v", hits)
		}
	}
}
