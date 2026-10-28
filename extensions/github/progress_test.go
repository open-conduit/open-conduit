package github_test

import (
	"testing"

	gh "github.com/open-conduit/open-conduit/extensions/github"
)

func TestChecksProgress(t *testing.T) {
	for _, c := range []struct {
		p    gh.ChecksPayload
		want string
	}{
		{gh.ChecksPayload{Runs: []gh.CheckRun{{State: "pending"}}}, "0 of 1 check finished"},
		{gh.ChecksPayload{Runs: []gh.CheckRun{{State: "success"}, {State: "pending"}, {State: "pending"}}}, "1 of 3 checks finished"},
		{gh.ChecksPayload{Runs: []gh.CheckRun{{State: "failure"}, {State: "success"}, {State: "pending"}}}, "2 of 3 checks finished, 1 failed"},
		// A required check that has not reported is unfinished.
		{gh.ChecksPayload{Runs: []gh.CheckRun{{State: "success"}}, Missing: []string{"deploy"}}, "1 of 2 checks finished"},
	} {
		if got := gh.ChecksProgress(c.p); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
}
