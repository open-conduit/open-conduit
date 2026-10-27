package github

import (
	"fmt"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
)

// Annotation is one line of operator context about Work this entry owns.
type Annotation struct{ Name, Value string }

// Web is the browser URL of the client's host (https://github.com, or an
// Enterprise host).
func (c *Client) Web() string { return c.web }

// Annotate describes Work owned by this entry for operator views: the issue
// and its URL, whether it is a held issue and projection dry run. It makes
// no service calls. ok is false for Work this entry does not own.
func Annotate(s Settings, web string, w domain.Work) ([]Annotation, bool) {
	n := 0
	for _, k := range w.CorrelationKeys {
		if k.Source != s.Namespace {
			continue
		}
		if num, _, ok := ParseIssueKey(k.ExternalID); ok {
			n = num
			break
		}
	}
	if n == 0 {
		return nil, false
	}
	out := []Annotation{
		{"issue", fmt.Sprintf("%s#%d", s.TrackerRepo, n)},
		{"url", fmt.Sprintf("%s/%s/issues/%d", strings.TrimRight(web, "/"), s.TrackerRepo, n)},
	}
	if w.Kind == WorkKindHeld && w.State == domain.WorkWaiting {
		out = append(out, Annotation{"held", "waiting for its time or conditions (scheduled label kept)"})
	}
	if s.Projection.DryRun {
		out = append(out, Annotation{"projection", "dry run: labels and comments are not written"})
	} else if s.Projection.Disabled {
		out = append(out, Annotation{"projection", "disabled"})
	}
	return out, true
}
