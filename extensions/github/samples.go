package github

import (
	"encoding/json"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Sample Events for a route trace ("Suppose…"), one set per github entry of
// the scope, written as the entry's normalizer would produce them for issue
// #1 and pull request #2.

var (
	_ extension.RouteSampler = Route{}
	_ extension.RouteSampler = HoldRoute{}
)

func scopeSettings(scope domain.Scope) []Settings {
	var out []Settings
	for _, x := range scopeExtensions(scope) {
		if x.Type != Type {
			continue
		}
		if s, err := parseSettings(x.Config, nil, false); err == nil && s.Scope == string(scope.ID) {
			out = append(out, s)
		}
	}
	return out
}

func sampleEvent(source, typ string, key domain.CorrelationKey, schema string, payload any) domain.Event {
	data, _ := json.Marshal(payload)
	return domain.Event{Type: typ, Source: source, CorrelationKey: &key, Data: domain.Payload{Schema: schema, Data: data}}
}

func sampleIssue(s Settings, labels []string) domain.Event {
	if labels == nil {
		labels = []string{}
	}
	return sampleEvent(s.Namespace, TypeIssueOpen, domain.CorrelationKey{Source: s.Namespace, ExternalID: IssueKey(1, "")}, SchemaIssue,
		IssuePayload{Repo: s.Repo, Number: 1, Title: "Example issue", State: "open", Labels: labels, Assignees: []string{}, Author: "octocat"})
}

// sampleID keeps samples of several entries in one scope apart.
func sampleID(all []Settings, s Settings, id string) string {
	if len(all) > 1 {
		return s.Namespace + "/" + id
	}
	return id
}

// Samples offers a new issue, a comment, checks finishing on the issue's
// pull request, and a merge.
func (Route) Samples(scope domain.Scope, _ domain.Route) []extension.RouteSample {
	var out []extension.RouteSample
	all := scopeSettings(scope)
	for _, s := range all {
		var labels []string
		if len(s.Labels.Require) > 0 {
			labels = []string{s.Labels.Require[0]}
		}
		issue := domain.CorrelationKey{Source: s.TrackerNS, ExternalID: IssueKey(1, "")}
		out = append(out,
			extension.RouteSample{ID: sampleID(all, s, "issue-opened"), Title: "A new issue is opened", Event: sampleIssue(s, labels)},
			extension.RouteSample{ID: sampleID(all, s, "comment"), Title: "Someone comments on an issue being worked", Event: sampleEvent(s.Namespace, TypeCommentHuman, issue, SchemaComment,
				CommentPayload{Repo: s.Repo, ID: 1, Kind: KindIssue, Number: 1, Author: "octocat", Body: "Could you also cover the edge case?"})},
			extension.RouteSample{ID: sampleID(all, s, "checks-passed"), Title: "Checks pass on a waiting pull request", Event: sampleEvent(s.Namespace, TypeChecksPassed, issue, SchemaChecks,
				ChecksPayload{Repo: s.Repo, Number: 2, SHA: "0000000", Complete: true, Failed: []string{}, Runs: []CheckRun{}})},
			extension.RouteSample{ID: sampleID(all, s, "pull-merged"), Title: "A pull request is merged", Event: sampleEvent(s.Namespace, TypePullMerged, domain.CorrelationKey{Source: s.Namespace, ExternalID: PullKey(2)}, SchemaPull,
				PullPayload{Repo: s.Repo, Number: 2, Title: "Example change", State: "closed", Merged: true, Author: "octocat"})},
		)
	}
	return out
}

// Samples offers a new issue carrying the scheduled label.
func (HoldRoute) Samples(scope domain.Scope, _ domain.Route) []extension.RouteSample {
	var out []extension.RouteSample
	all := scopeSettings(scope)
	for _, s := range all {
		labels := []string{s.Labels.Scheduled}
		if len(s.Labels.Require) > 0 {
			labels = append(labels, s.Labels.Require[0])
		}
		out = append(out, extension.RouteSample{ID: sampleID(all, s, "held-issue-opened"), Title: "A new issue labelled " + s.Labels.Scheduled + " is opened", Event: sampleIssue(s, labels)})
	}
	return out
}
