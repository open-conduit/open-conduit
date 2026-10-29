package github

import "testing"

// A PR body gets `Closes #N` unless it already names the issue, so one PR of
// a multi-PR build that says `Refs #N` doesn't close the issue on merge.
func TestLinkBody(t *testing.T) {
	own := Settings{Repo: "acme/web", TrackerRepo: "acme/web"}
	shared := Settings{Repo: "acme/web", TrackerRepo: "acme/tracker", SharedTracker: true}
	for name, c := range map[string]struct {
		s          Settings
		body, want string
	}{
		"plain":                 {own, "Steadies the header.", "Closes #7\n\nSteadies the header."},
		"refs":                  {own, "Step one.\n\nRefs #7", "Step one.\n\nRefs #7"},
		"closes":                {own, "Fixes #7 at last.", "Fixes #7 at last."},
		"own repo by name":      {own, "Refs acme/web#7", "Refs acme/web#7"},
		"another issue":         {own, "Refs #8", "Closes #7\n\nRefs #8"},
		"another repo":          {own, "Refs other/web#7", "Closes #7\n\nRefs other/web#7"},
		"a mention, no keyword": {own, "Like #7.", "Closes #7\n\nLike #7."},
		"shared tracker":        {shared, "Refs acme/tracker#7", "Refs acme/tracker#7"},
		"shared, bare number":   {shared, "Refs #7", "Closes acme/tracker#7\n\nRefs #7"},
	} {
		if got := linkBody(c.s, 7, c.body); got != c.want {
			t.Errorf("%s: %q", name, got)
		}
	}
}
