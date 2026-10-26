package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

func handlerClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg, err := ParseConnection([]byte(`{"api_url":"` + srv.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(cfg, secrets(map[string]string{SecretToken: "tok"}), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Port of v1 c37edf1 adapters/github TestRequiredChecks: protection and
// rulesets are both read, deduplicated, with an escaped branch name, and a
// plan without rulesets (403) is a refused source, not an error.
func TestRequiredChecksFromProtectionAndRulesets(t *testing.T) {
	c := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.Split(r.URL.EscapedPath(), "?")[0] {
		case "/repos/octo/widgets/branches/release/v%231":
			fmt.Fprint(w, `{"protection":{"required_status_checks":{"contexts":["netlify/site/deploy-preview"],"checks":[{"context":"netlify/site/deploy-preview"},{"context":"test"}]}}}`)
		case "/repos/octo/widgets/rules/branches/release/v%231":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`)
		case "/repos/octo/widgets/branches/main":
			fmt.Fprint(w, `{"protection":{"enabled":false,"required_status_checks":{"contexts":[],"checks":[]}}}`)
		case "/repos/octo/widgets/rules/branches/main":
			fmt.Fprint(w, `[{"type":"deletion"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"typecheck"}]}}]`)
		case "/repos/octo/widgets/branches/broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			t.Errorf("unexpected %s", r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	got, err := c.RequiredChecks(context.Background(), "octo/widgets", "release/v#1")
	if err != nil || strings.Join(got.Names, ",") != "netlify/site/deploy-preview,test" ||
		strings.Join(got.From, ",") != RequiredFromProtection || strings.Join(got.Refused, ",") != RequiredFromRulesets {
		t.Fatalf("protected branch: %+v %v", got, err)
	}
	got, err = c.RequiredChecks(context.Background(), "octo/widgets", "main")
	if err != nil || strings.Join(got.Names, ",") != "typecheck" || len(got.From) != 2 || len(got.Refused) != 0 {
		t.Fatalf("ruleset: %+v %v", got, err)
	}
	if _, err = c.RequiredChecks(context.Background(), "octo/widgets", "broken"); err == nil {
		t.Fatal("a failed read must be an error, never an absence of requirements")
	}
}

// A refused protection read (a fine-grained token without the permission)
// degrades to the rulesets and says so; a rate limit is an error.
func TestRequiredChecksRefusedAndRateLimited(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	clk := &clock{t: time.Now()}
	client := tokenClient(t, fake, clk, "tok")
	fake.RequireChecks("main", "build")
	fake.RequireRulesetChecks("main", "typecheck", "build")
	got, err := client.RequiredChecks(context.Background(), "octo/widgets", "main")
	if err != nil || strings.Join(got.Names, ",") != "build,typecheck" {
		t.Fatalf("both sources: %+v %v", got, err)
	}
	fake.RefuseRequiredChecks(true, false)
	got, err = client.RequiredChecks(context.Background(), "octo/widgets", "main")
	if err != nil || strings.Join(got.Names, ",") != "typecheck,build" || strings.Join(got.Refused, ",") != RequiredFromProtection {
		t.Fatalf("refused protection: %+v %v", got, err)
	}
	if note := RequiredNote(got, RunsFromCheckRuns); !strings.Contains(note, "read from rulesets only") || !strings.Contains(note, "refused branch_protection") {
		t.Fatalf("note %q", note)
	}
	if note := RequiredNote(got, RunsFromWorkflows); !strings.Contains(note, "cannot be verified") {
		t.Fatalf("workflow-runs note %q", note)
	}
	fake.RateLimitPrimary(clk.now().Add(time.Minute))
	if _, err = client.RequiredChecks(context.Background(), "octo/widgets", "main"); err == nil || IsStatus(err, http.StatusForbidden) {
		t.Fatalf("rate limit must be an error, not a refusal: %v", err)
	}
}

// Port of v1 c37edf1 core TestChecksMissing.
func TestMissingChecks(t *testing.T) {
	runs := []CheckRun{{Name: "test", State: "success"}, {Name: "lint", State: "failure"}}
	got := missingChecks(runs, []string{"test", "deploy-preview", "lint", "deploy-preview"})
	if len(got) != 1 || got[0] != "deploy-preview" {
		t.Fatalf("missing %v", got)
	}
}
