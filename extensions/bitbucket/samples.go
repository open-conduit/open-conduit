package bitbucket

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// Sample Events for a route trace ("Suppose…"), one set per bitbucket entry
// of the scope, written as the entry's normalizer would produce them for
// pull request 2 of issue 7. The route never starts Work, so these trace
// as input once the issue has Work and as a skip before.

var _ extension.RouteSampler = Route{}

func scopeSettings(scope domain.Scope) []Settings {
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
		if x.Type != Type {
			continue
		}
		if s, err := parseSettings(x.Config, nil, false); err == nil && s.Scope == string(scope.ID) {
			out = append(out, s)
		}
	}
	return out
}

func sampleEvent(s Settings, typ string, key domain.CorrelationKey, schema string, payload any) domain.Event {
	data, _ := json.Marshal(payload)
	return domain.Event{Type: typ, Source: s.Namespace, CorrelationKey: &key, Data: domain.Payload{Schema: schema, Data: data}}
}

// Samples offers a comment, finished checks and a merge on the pull request
// of an issue.
func (Route) Samples(scope domain.Scope, _ domain.Route) []extension.RouteSample {
	var out []extension.RouteSample
	all := scopeSettings(scope)
	for _, s := range all {
		id := func(id string) string {
			if len(all) > 1 {
				return s.Namespace + "/" + id
			}
			return id
		}
		prefix := "PROJ"
		if len(s.TrackerKeys) > 0 {
			prefix = s.TrackerKeys[0]
		}
		issue := domain.CorrelationKey{Source: s.TrackerNS, ExternalID: tracker.IssueKey(prefix+"-7", "")}
		now := time.Now().UTC().Truncate(time.Second)
		out = append(out,
			extension.RouteSample{ID: id("comment"), Title: "Someone comments on a pull request", Event: sampleEvent(s, TypeCommentHuman, issue, SchemaComment,
				CommentPayload{Repo: s.Repo, Number: 2, ID: 1, Author: "Example Person", Body: "Could you also cover the edge case?", CreatedAt: now, UpdatedAt: now})},
			extension.RouteSample{ID: id("checks-failed"), Title: "Checks fail on a pull request", Event: sampleEvent(s, TypeChecksFailed, issue, SchemaChecks,
				ChecksPayload{Repo: s.Repo, Number: 2, SHA: "0000000", State: "failed", Runs: []CheckRun{{Name: "build", State: "FAILED"}}, Failed: []string{"build"}})},
			extension.RouteSample{ID: id("pull-merged"), Title: "A pull request is merged", Event: sampleEvent(s, TypePullMerged, issue, SchemaPull,
				PullPayload{Repo: s.Repo, Number: 2, Title: "Example change", State: "merged", Author: "Example Person", Issue: prefix + "-7", CreatedAt: now})},
		)
	}
	return out
}
