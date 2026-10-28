package github

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Work display (issue #123): what an issue's Work is, for operator views.
// An issue Event gives its title, number, author and link; input about one
// of its PRs adds the PR's link. Input that says nothing new keeps the
// display as it is.

// display is the Work's display after Event e, or nil to keep it.
func display(w *domain.Work, e domain.Event) *domain.WorkDisplay {
	var cur domain.WorkDisplay
	if w != nil && w.Display != nil {
		cur = *w.Display
		cur.Links = append([]domain.Link(nil), cur.Links...)
	}
	switch e.Data.Schema {
	case SchemaIssue:
		var p IssuePayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Number <= 0 {
			return nil
		}
		next := cur
		next.Title = extension.Clip(p.Title, extension.MaxDisplayTitle)
		next.Ref = fmt.Sprintf("#%d", p.Number)
		next.Requester = extension.Clip(p.Author, extension.MaxDisplayRequester)
		next.Links = nil
		next = withLink(next, fmt.Sprintf("Issue #%d", p.Number), p.URL)
		for _, l := range cur.Links {
			next = withLink(next, l.Label, l.URL)
		}
		return changed(w, next)
	case SchemaPull:
		var p PullPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Number <= 0 {
			return nil
		}
		return changed(w, withLink(cur, fmt.Sprintf("PR #%d", p.Number), p.URL))
	case SchemaChecks:
		var p ChecksPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Number <= 0 {
			return nil
		}
		return changed(w, withLink(cur, fmt.Sprintf("PR #%d", p.Number), pullURL(cur, p.Repo, p.Number)))
	case SchemaComment:
		var p CommentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Kind == KindIssue || p.Number <= 0 {
			return nil
		}
		return changed(w, withLink(cur, fmt.Sprintf("PR #%d", p.Number), pullURL(cur, p.Repo, p.Number)))
	}
	return nil
}

// withLink adds a link unless one with its label is there, the URL cannot
// be a display link (extension.DisplayURL), or the display is full.
func withLink(d domain.WorkDisplay, label, url string) domain.WorkDisplay {
	if !extension.DisplayURL(url) || len(d.Links) >= extension.MaxDisplayLinks {
		return d
	}
	for _, l := range d.Links {
		if l.Label == label {
			return d
		}
	}
	d.Links = append(d.Links, domain.Link{Label: label, URL: url})
	return d
}

// pullURL is a PR's browser URL on the host of the display's issue link
// (https://github.com/o/r/issues/1 gives https://github.com), or "" when
// the display has none to go by.
func pullURL(d domain.WorkDisplay, repo string, number int) string {
	for _, l := range d.Links {
		i := strings.Index(l.URL, "/issues/")
		if !strings.HasPrefix(l.Label, "Issue ") || i < 0 || repo == "" {
			continue
		}
		base := l.URL[:i]
		for n := 0; n < 2; n++ {
			if j := strings.LastIndex(base, "/"); j > len("https://") {
				base = base[:j]
			}
		}
		return fmt.Sprintf("%s/%s/pull/%d", base, repo, number)
	}
	return ""
}

// changed is next, or nil when it is what the Work already shows or says
// nothing a display can hold.
func changed(w *domain.Work, next domain.WorkDisplay) *domain.WorkDisplay {
	if next.Title == "" && next.Ref == "" {
		return nil
	}
	if w != nil && w.Display != nil && equalDisplay(*w.Display, next) {
		return nil
	}
	return &next
}

func equalDisplay(a, b domain.WorkDisplay) bool {
	if a.Title != b.Title || a.Ref != b.Ref || a.Requester != b.Requester || len(a.Links) != len(b.Links) {
		return false
	}
	for i := range a.Links {
		if a.Links[i] != b.Links[i] {
			return false
		}
	}
	return true
}
