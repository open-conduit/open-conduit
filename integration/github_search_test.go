package integration_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

// Work that owns no issue (a Slack question handed on) searches the
// repository's PRs, whoever opened them, newest first, and filters them by
// state, author, head and base; github.pr.list still lists only its own.
func TestGitHubPRSearchReadsTheRepositorysPRs(t *testing.T) {
	e := newToolsEnv(t)
	bob := githubtest.User{Login: "bob", Type: "User"}
	var prs []int
	for i, who := range []githubtest.User{ghAlice, bob, ghAlice, bob, ghAlice, bob, ghAlice} {
		prs = append(prs, e.fake.OpenPull("Change "+itoa(i+1), "A long body nobody asked for.", "feature-"+itoa(i+1), "s"+itoa(i+1), who))
	}
	e.fake.MergePull(prs[0])
	e.fake.CloseIssue(prs[1], "")
	e.fake.SetDraft(prs[6], true)
	e.fake.SetBase(prs[5], "release")
	w := e.request("thread-search")
	a := e.start(w.ID)
	defer e.finish(a, nil)

	numbers := func(out map[string]any) []int {
		var ns []int
		for _, p := range out["prs"].([]any) {
			ns = append(ns, int(p.(map[string]any)["number"].(float64)))
		}
		return ns
	}
	search := func(in map[string]any) map[string]any { return e.must(a, gh.OpPRSearch, "", in) }

	if mine := e.must(a, gh.OpPRList, "", map[string]any{}); len(mine["prs"].([]any)) != 0 {
		t.Fatalf("github.pr.list lists others' PRs: %v", mine)
	}
	// The five newest PRs, newest first; there are more.
	out := search(map[string]any{"state": "all", "limit": 5})
	if got, want := numbers(out), []int{prs[6], prs[5], prs[4], prs[3], prs[2]}; !slices.Equal(got, want) || out["more"] != true || out["repo"] != ghRepo {
		t.Fatalf("five newest: %v more=%v, want %v", got, out["more"], want)
	}
	first := out["prs"].([]any)[0].(map[string]any)
	if first["draft"] != true || first["author"] != ghAlice.Login || first["branch"] != "feature-7" || first["base"] != "main" ||
		first["state"] != "open" || first["created_at"] == nil || first["updated_at"] == nil || first["url"] == nil {
		t.Fatalf("summary: %v", first)
	}
	if _, ok := first["body"]; ok {
		t.Fatalf("a search returns bodies: %v", first)
	}
	if out := search(map[string]any{}); len(numbers(out)) != 5 || out["more"] != false {
		t.Fatalf("every open PR: %v", out)
	}

	for _, tc := range []struct {
		in   map[string]any
		want []int
	}{
		{map[string]any{"state": "merged"}, []int{prs[0]}},
		{map[string]any{"state": "closed"}, []int{prs[1]}},
		{map[string]any{"state": "all", "author": "BOB"}, []int{prs[5], prs[3], prs[1]}},
		{map[string]any{"head": "feature-4"}, []int{prs[3]}},
		{map[string]any{"base": "release"}, []int{prs[5]}},
		// Updated most recently: the retargeted PR, then the draft.
		{map[string]any{"sort": "updated", "limit": 2}, []int{prs[5], prs[6]}},
	} {
		if got := numbers(search(tc.in)); !slices.Equal(got, tc.want) {
			t.Fatalf("%v: %v, want %v", tc.in, got, tc.want)
		}
	}
	merged := search(map[string]any{"state": "merged"})["prs"].([]any)[0].(map[string]any)
	if merged["state"] != "merged" || merged["merged_at"] == nil {
		t.Fatalf("merged: %v", merged)
	}

	for _, in := range []map[string]any{{"state": "draft"}, {"sort": "popularity"}, {"limit": 101}, {"limit": -1}, {"bodies": true}} {
		_, err := e.call(a, gh.OpPRSearch, "", in)
		refused(t, err, "invalid_input")
	}
}

// The repository is one the scope registers, named when it registers
// several: the same rule as github.pr.view.
func TestGitHubPRSearchNeedsARegisteredRepository(t *testing.T) {
	e := newToolsEnv(t)
	e.fake.OpenPull("Change", "", "feature", "h1", ghAlice)
	w := e.request("thread-search-repos")
	a := e.start(w.ID)
	defer e.finish(a, nil)
	_, err := e.call(a, gh.OpPRSearch, "", map[string]any{"repo": "octo/elsewhere"})
	refused(t, err, "unregistered_repo")
	s, err := gh.ParseExtension([]byte(`{"namespace":"github/github.com/octo/gadgets","scope":"`+ghScope+`","repo":"octo/gadgets"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.extra = []gh.Source{{Settings: s, Client: e.client, Projector: e.projector}}
	e.mu.Unlock()
	_, err = e.call(a, gh.OpPRSearch, "", map[string]any{})
	refused(t, err, "repo_required")
	_, err = e.call(a, gh.OpPRSearch, "", map[string]any{"repo": "octo/elsewhere"})
	refused(t, err, "unregistered_repo")
	if out := e.must(a, gh.OpPRSearch, "", map[string]any{"repo": ghRepo}); len(out["prs"].([]any)) != 1 {
		t.Fatalf("named repo: %v", out)
	}
}

// The wide read is granted on its own: a binding granted the other GitHub
// operations but not github.pr.search neither lists it nor may call it, and
// the refusal is the host's. Negative control: the same binding granted it
// may.
func TestGitHubPRSearchIsGrantedOnItsOwn(t *testing.T) {
	without := slices.DeleteFunc(gh.Operations(), func(op string) bool { return op == gh.OpPRSearch })
	doc := func(ops []string) string {
		ops = append([]string{extension.ReadContext, extension.Emit, extension.Wait, extension.DeclareOutcome}, ops...)
		b, _ := json.Marshal(map[string]any{"version": 1, "settings": map[string]any{}, "connections": []any{},
			"scopes": []any{map[string]any{"id": ghScope, "name": ghRepo,
				"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []string{}},
				"bindings": []any{map[string]any{"name": "agent", "executor": "fixture", "requires": map[string]any{"toolbelt": "cli"},
					"operations": ops, "secret_names": []string{}, "config": map[string]any{}}},
				"routes":      []any{map[string]any{"name": "issues", "handler": "github", "binding": "agent", "active_policy": "queue", "config": map[string]any{}}},
				"secret_refs": map[string]any{}}}})
		return string(b)
	}
	for _, granted := range []bool{false, true} {
		ops := without
		if granted {
			ops = gh.Operations()
		}
		e := newToolsEnvWith(t, doc(ops))
		e.fake.OpenPull("Change", "", "feature", "h1", ghAlice)
		_, w := e.claim("Header wobbles", "", "bug")
		a := e.start(w.ID)
		_, err := e.call(a, gh.OpPRSearch, "", map[string]any{})
		if granted && err != nil {
			t.Fatalf("a granted github.pr.search: %v", err)
		}
		if !granted && !errors.Is(err, extension.ErrUnauthorized) {
			t.Fatalf("an ungranted github.pr.search: %v", err)
		}
		e.finish(a, nil)
	}
}

// A filter GitHub's list can't apply (author) reads on past its first page
// of a hundred, and stops at the first match past the limit.
func TestGitHubPRSearchFiltersAcrossPages(t *testing.T) {
	e := newToolsEnv(t)
	bob := githubtest.User{Login: "bob", Type: "User"}
	var bobs []int
	for i := 0; i < 160; i++ {
		who := ghAlice
		if i < 3 {
			who = bob
		}
		n := e.fake.OpenPull("Change "+itoa(i), "", "feature-"+itoa(i), "s"+itoa(i), who)
		if who == bob {
			bobs = append(bobs, n)
		}
	}
	w := e.request("thread-search-pages")
	a := e.start(w.ID)
	defer e.finish(a, nil)
	out := e.must(a, gh.OpPRSearch, "", map[string]any{"author": "bob", "limit": 2})
	ps := out["prs"].([]any)
	if len(ps) != 2 || int(ps[0].(map[string]any)["number"].(float64)) != bobs[2] || out["more"] != true {
		t.Fatalf("bob's newest two: %v", out)
	}
	if out := e.must(a, gh.OpPRSearch, "", map[string]any{"author": "bob"}); len(out["prs"].([]any)) != 3 || out["more"] != false {
		t.Fatalf("all of bob's: %v", out)
	}
}
