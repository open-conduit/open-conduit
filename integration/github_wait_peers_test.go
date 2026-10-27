package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

// One agent's comment on another agent's issue wakes that agent's parked
// Work; the parked agent's own reason comment does not.
func TestGitHubPeerCommentWakesAParkedAgent(t *testing.T) {
	e := newToolsEnv(t)
	n, w := e.claim("Wave two", "", "bug")
	_, peer := e.claim("Wave one", "", "bug")
	a := e.start(w.ID)
	later := e.clock.Now().Add(90 * 24 * time.Hour).UTC().Format("2006-01-02T15:04Z")
	if _, err := e.call(a, gh.OpWait, "park", map[string]any{"until": later, "reason": "Paused until released."}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("own reason comment woke it: %v", got.State)
	}
	b := e.start(peer.ID)
	e.must(b, gh.OpIssueComment, "release", map[string]any{"number": n, "body": "Released: start now."})
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("peer comment did not wake: %v", got.State)
	}
	// The woken turn carries the peer's comment, and the wake says why.
	queued := e.queued(w.ID)
	if !contains(queued, gh.TypeCommentAgent) || !contains(queued, gh.TypeDependencyMet) {
		t.Fatalf("woken turn's input: %v", queued)
	}
	for _, ev := range e.events() {
		if ev.Event.Type == gh.TypeDependencyMet && ev.Event.WorkID == w.ID {
			if text, ok := gh.RenderEvent(ev.Event); !ok || !strings.Contains(text, "Another agent commented while you waited") {
				t.Fatalf("wake reads:\n%s", text)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Parking again with only a time keeps what the issue was held for: the
// held-for PR merging still wakes the agent before the time comes.
func TestGitHubWaitKeepsTheHeldDependency(t *testing.T) {
	e := newToolsEnv(t)
	pr := e.fake.OpenPull("Schema", "", "elsewhere", "s70", ghAlice)
	n, w := e.claim("Follow-up", "After the schema.\n\n<!-- waitFor: pr:"+itoa(pr)+":merged -->", "bug")
	e.sync()
	a := e.start(w.ID)
	later := e.clock.Now().Add(90 * 24 * time.Hour).UTC().Format("2006-01-02T15:04Z")
	out := e.must(a, gh.OpWait, "park", map[string]any{"until": later, "reason": "Paused."})
	if kept, _ := out["kept"].([]any); len(kept) != 1 || kept[0] != "pr:"+itoa(pr)+":merged" {
		t.Fatalf("kept: %v", out)
	}
	mine := e.agentComments(n)
	if len(mine) != 1 || !strings.Contains(mine[0].Body, "waitFor: pr:"+itoa(pr)+":merged; until") {
		t.Fatalf("reason comment: %+v", mine)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("not parked: %v", got.State)
	}
	e.fake.MergePull(pr)
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("held-for PR merging did not wake: %v", got.State)
	}

	// Once it has happened, the next wait lets it go.
	a = e.start(w.ID)
	out = e.must(a, gh.OpWait, "park-again", map[string]any{"until": later, "reason": "Paused again."})
	if out["kept"] != nil {
		t.Fatalf("kept a met condition: %v", out)
	}
}
