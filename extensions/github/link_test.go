package github

import "testing"

// A PR body gets `Closes #N` (or `Refs #N` when the PR shouldn't close the
// issue) unless it already names the issue with a keyword of its own.
func TestLinkBody(t *testing.T) {
	own := Settings{Repo: "acme/web", TrackerRepo: "acme/web"}
	shared := Settings{Repo: "acme/web", TrackerRepo: "acme/tracker", SharedTracker: true}
	for name, c := range map[string]struct {
		s          Settings
		body, want string
		keep       bool
	}{
		"plain":                 {own, "Steadies the header.", "Closes #7\n\nSteadies the header.", false},
		"refs":                  {own, "Step one.\n\nRefs #7", "Step one.\n\nRefs #7", false},
		"closes":                {own, "Fixes #7 at last.", "Fixes #7 at last.", false},
		"own repo by name":      {own, "Refs acme/web#7", "Refs acme/web#7", false},
		"another issue":         {own, "Refs #8", "Closes #7\n\nRefs #8", false},
		"another repo":          {own, "Refs other/web#7", "Closes #7\n\nRefs other/web#7", false},
		"a mention, no keyword": {own, "Like #7.", "Closes #7\n\nLike #7.", false},
		"shared tracker":        {shared, "Refs acme/tracker#7", "Refs acme/tracker#7", false},
		"shared, bare number":   {shared, "Refs #7", "Closes acme/tracker#7\n\nRefs #7", false},
		"keep open":             {own, "Step one.", "Refs #7\n\nStep one.", true},
		"keep open, shared":     {shared, "Step one.", "Refs acme/tracker#7\n\nStep one.", true},
		"keep open, named":      {own, "Fixes #7 at last.", "Fixes #7 at last.", true},
	} {
		if got := linkBody(c.s, 7, c.body, !c.keep); got != c.want {
			t.Errorf("%s: %q", name, got)
		}
	}
}
