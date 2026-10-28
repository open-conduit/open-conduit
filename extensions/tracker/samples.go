package tracker

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Sample Events for a route trace ("Suppose…"), one set per entry of the
// tracker's type in the scope, written as the entry's normalizer would
// produce them for issue ENG-1. A new issue carries a required label and
// the first value of each attribute the route's match asks for, so it
// traces as the route's own kind of issue.

var _ extension.RouteSampler = Route{}

func (r Route) scopeSettings(scope domain.Scope) []Settings {
	var meta struct {
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return nil
	}
	var out []Settings
	for _, x := range meta.Extensions {
		if x.Type != r.Type {
			continue
		}
		if s, err := ParseLenient(r.Type, x.Config); err == nil && s.Scope == string(scope.ID) {
			out = append(out, s)
		}
	}
	return out
}

func sampleEvent(s Settings, typ, schema string, payload any) domain.Event {
	data, _ := json.Marshal(payload)
	return domain.Event{Type: typ, Source: s.Namespace,
		CorrelationKey: &domain.CorrelationKey{Source: s.Namespace, ExternalID: IssueKey("ENG-1", "")},
		Data:           domain.Payload{Schema: schema, Data: data}}
}

// Samples offers a new issue, a comment on an issue being worked and the
// issue closing.
func (r Route) Samples(_ context.Context, in extension.SampleInput) []extension.RouteSample {
	scope, route := in.Scope, in.Route
	t := TypesOf(r.Type)
	var match map[string][]string
	if len(route.Config.Data) > 0 {
		if rc, err := ParseRoute(route.Config.Data); err == nil {
			match = rc.Match
		}
	}
	var out []extension.RouteSample
	all := r.scopeSettings(scope)
	for _, s := range all {
		id := func(id string) string {
			if len(all) > 1 {
				return s.Namespace + "/" + id
			}
			return id
		}
		now := time.Now().UTC().Truncate(time.Second)
		issue := IssuePayload{Key: "ENG-1", Title: "Example issue", State: "open", Labels: []string{}, Attrs: map[string][]string{}, Author: "Example Person", CreatedAt: now}
		if len(s.Labels.Require) > 0 {
			issue.Labels = append(issue.Labels, s.Labels.Require[0])
		}
		names := make([]string, 0, len(match))
		for n := range match {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v := match[n][0]
			switch n {
			case "labels":
				issue.Labels = append(issue.Labels, v)
			case "status":
				issue.Status = v
			case "title":
				issue.Title = v
			default:
				issue.Attrs[n] = []string{v}
			}
		}
		out = append(out,
			extension.RouteSample{ID: id("issue-opened"), Title: "A new issue is opened", Event: sampleEvent(s, t.IssueOpen, t.SchemaIssue, issue)},
			extension.RouteSample{ID: id("comment"), Title: "Someone comments on an issue being worked", Event: sampleEvent(s, t.CommentHuman, t.SchemaComment,
				CommentPayload{Issue: "ENG-1", ID: "comment-1", Author: "Example Person", Body: "Could you also cover the edge case?", CreatedAt: now, UpdatedAt: now})},
			extension.RouteSample{ID: id("issue-closed"), Title: "An issue being worked is closed", Event: sampleEvent(s, t.IssueClosed, t.SchemaIssue,
				IssuePayload{Key: "ENG-1", Title: "Example issue", State: "closed", Labels: []string{}, CreatedAt: now})},
		)
	}
	return out
}
