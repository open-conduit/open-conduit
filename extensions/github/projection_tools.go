package github

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// IssueIntent is one issue an agent opens (follow-up child work). Like a
// comment it is not re-derivable: it has a stable ID, a durable record and a
// discoverable marker in its body, so an ambiguous create is found again
// instead of opening a duplicate.
type IssueIntent struct {
	ID     string
	Work   domain.ID
	Title  string
	Body   string
	Labels []string
}

// PostedIssue identifies an issue CreateIssue opened.
type PostedIssue struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type issueRecord struct {
	ID          string    `json:"id"`
	Work        domain.ID `json:"work"`
	Repo        string    `json:"repo"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Labels      []string  `json:"labels,omitempty"`
	State       string    `json:"state"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	Number      int       `json:"number,omitempty"`
	URL         string    `json:"url,omitempty"`
}

// CreateIssue opens an issue in the tracker repository at most once per
// intent, as observed through its marker. It returns the issue once opened,
// with the durable state ("posted", or "attempted"/"pending" while an
// ambiguous create waits for its marker search; Converge finishes it).
func (p *Projector) CreateIssue(ctx context.Context, in IssueIntent) (PostedIssue, string, error) {
	if in.ID == "" || strings.TrimSpace(in.Title) == "" {
		return PostedIssue{}, "", storage.ErrInvalid
	}
	repo, key := p.o.Settings.TrackerRepo, "issue-intent/"+digest(in.ID)[:40]
	var rec issueRecord
	err := p.transact(ctx, func(tx storage.Tx) error {
		found, err := p.get(tx, key, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = issueRecord{ID: in.ID, Work: in.Work, Repo: repo, Title: in.Title, Body: in.Body, Labels: in.Labels, State: intentPending, CreatedAt: p.o.Now().UTC()}
			return p.put(tx, key, rec)
		}
		if rec.Title != in.Title || rec.Body != in.Body || rec.Repo != repo {
			return ErrIntentConflict
		}
		return nil
	})
	if err != nil || rec.State == intentPosted {
		return PostedIssue{Number: rec.Number, URL: rec.URL}, rec.State, err
	}
	return p.openIssue(ctx, key, rec)
}

func (p *Projector) openIssue(ctx context.Context, key string, rec issueRecord) (PostedIssue, string, error) {
	marker := markerID(rec.ID)
	if rec.State == intentAttempted {
		if p.o.Now().Sub(rec.AttemptedAt) < p.o.SearchDelay {
			return PostedIssue{}, rec.State, nil
		}
		issues, err := p.o.Client.RecentIssues(ctx, rec.Repo, rec.CreatedAt.Add(-time.Minute))
		if err != nil {
			return PostedIssue{}, rec.State, err
		}
		for _, i := range issues {
			if i.PullRequest == nil && hasIntent(i.Body, marker) {
				return p.issueOpened(ctx, key, i.Number, i.HTMLURL)
			}
		}
	}
	if p.o.Settings.Projection.DryRun {
		return PostedIssue{}, rec.State, nil
	}
	err := p.transact(ctx, func(tx storage.Tx) error {
		var cur issueRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.Attempts, cur.AttemptedAt = intentAttempted, cur.Attempts+1, p.o.Now().UTC()
		return p.put(tx, key, cur)
	})
	if err != nil {
		return PostedIssue{}, rec.State, err
	}
	i, err := p.o.Client.CreateIssue(ctx, rec.Repo, rec.Title, WithMarker(rec.Body, string(rec.Work), marker), rec.Labels)
	if err != nil {
		return PostedIssue{}, intentAttempted, err
	}
	return p.issueOpened(ctx, key, i.Number, i.HTMLURL)
}

func (p *Projector) issueOpened(ctx context.Context, key string, number int, url string) (PostedIssue, string, error) {
	err := p.transact(ctx, func(tx storage.Tx) error {
		var cur issueRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		cur.State, cur.Number, cur.URL = intentPosted, number, url
		return p.put(tx, key, cur)
	})
	if err != nil {
		return PostedIssue{}, intentAttempted, err
	}
	return PostedIssue{Number: number, URL: url}, intentPosted, nil
}

// redriveIssues finishes issue creations whose caller is gone.
func (p *Projector) redriveIssues(ctx context.Context, limit int) error {
	var pending []struct {
		key string
		rec issueRecord
	}
	if err := p.transact(ctx, func(tx storage.Tx) error {
		rows, err := tx.List(p.ns, "issue-intent/")
		for _, row := range rows {
			var rec issueRecord
			if json.Unmarshal(row.Value, &rec) == nil && rec.State != intentPosted {
				pending = append(pending, struct {
					key string
					rec issueRecord
				}{row.Key, rec})
			}
		}
		return err
	}); err != nil {
		return err
	}
	for i, x := range pending {
		if i >= limit {
			return nil
		}
		if _, _, err := p.openIssue(ctx, x.key, x.rec); err != nil {
			return err
		}
	}
	return nil
}

// Turn is a TypeAgentTurn event the host publishes for a finished agent
// turn, with the causal parent it derives from (the attempt's trigger).
type Turn struct {
	Scope  domain.ID
	Parent domain.ID
	Event  domain.Event
}

// Turns returns agent turns that ended with a declared status and have not
// been reported yet: the source of `issue:N:<state>` waits. Turn events are
// derived from durable outcomes, so a host crash only re-derives the same
// delivery identity. The host accepts them as derived Events and records
// each with MarkTurn.
func (p *Projector) Turns(ctx context.Context, limit int) ([]Turn, error) {
	var out []Turn
	err := p.transact(ctx, func(tx storage.Tx) error {
		out = nil
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope {
				continue
			}
			var key domain.CorrelationKey
			number := 0
			for _, k := range w.CorrelationKeys {
				if n, _, ok := ParseIssueKey(k.ExternalID); ok && k.Source == p.o.Settings.Namespace {
					key, number = k, n
					break
				}
			}
			if number == 0 {
				continue
			}
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
			for i := range xs {
				x := xs[i]
				d, ok := declaration(&x)
				if !ok || !validTurnState(d.Status) || x.State == domain.ExecutionRunning || x.State == domain.ExecutionPending {
					continue
				}
				if found, err := p.get(tx, "turn/"+string(x.ID), new(bool)); err != nil {
					return err
				} else if found {
					continue
				}
				data, _ := json.Marshal(TurnPayload{Repo: p.o.Settings.TrackerRepo, Number: number, State: d.Status, Work: string(w.ID), Execution: string(x.ID)})
				at := x.UpdatedAt
				if x.FinishedAt != nil {
					at = *x.FinishedAt
				}
				k := key
				delivery := "turn/" + string(x.ID)
				out = append(out, Turn{Scope: w.ScopeID, Parent: x.TriggerID, Event: domain.Event{
					ID: domain.ID("github-" + digest(p.o.Settings.Namespace, delivery)[:40]), Type: TypeAgentTurn, Source: p.o.Settings.Namespace,
					DeliveryID: delivery, WorkID: w.ID, CorrelationKey: &k, OccurredAt: at.UTC(), ReceivedAt: p.o.Now().UTC(),
					Data: domain.Payload{Schema: SchemaTurn, Data: data}}})
				if len(out) >= limit {
					return nil
				}
			}
		}
		return nil
	})
	return out, err
}

// MarkTurn records a reported turn so later passes skip it.
func (p *Projector) MarkTurn(ctx context.Context, execution domain.ID) error {
	return p.transact(ctx, func(tx storage.Tx) error { return p.put(tx, "turn/"+string(execution), true) })
}

func validTurnState(s string) bool {
	for _, t := range TurnStates {
		if s == t {
			return true
		}
	}
	return false
}
