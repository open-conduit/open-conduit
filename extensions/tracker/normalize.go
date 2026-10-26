package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Event family suffixes. A tracker's families are its type plus a suffix
// ("linear.issue.open"). Types name observed state, not webhook actions,
// because reconcile sees state; author class is in the type so a Wait can
// filter agent and bot notes out without a payload predicate.
const (
	SuffixIssueOpen      = ".issue.open"
	SuffixIssueClosed    = ".issue.closed"
	SuffixCommentHuman   = ".comment.human"
	SuffixCommentBot     = ".comment.bot"
	SuffixCommentAgent   = ".comment.agent"
	SuffixDelivery       = ".delivery"
	SuffixClaimRequested = ".claim.requested"
)

// Schemas are versioned per tracker type ("linear.issue.v1").
const (
	schemaIssue    = ".issue.v1"
	schemaComment  = ".comment.v1"
	schemaDelivery = ".delivery.v1"
)

// Types are one tracker's event family names.
type Types struct {
	IssueOpen, IssueClosed, CommentHuman, CommentBot, CommentAgent, Delivery, ClaimRequested string
	SchemaIssue, SchemaComment, SchemaDelivery                                               string
}

// TypesOf returns the family names of a tracker type.
func TypesOf(typ string) Types {
	return Types{IssueOpen: typ + SuffixIssueOpen, IssueClosed: typ + SuffixIssueClosed, CommentHuman: typ + SuffixCommentHuman,
		CommentBot: typ + SuffixCommentBot, CommentAgent: typ + SuffixCommentAgent, Delivery: typ + SuffixDelivery,
		ClaimRequested: typ + SuffixClaimRequested, SchemaIssue: typ + schemaIssue, SchemaComment: typ + schemaComment,
		SchemaDelivery: typ + schemaDelivery}
}

// DeliveryLoss is the declared loss limitation of raw webhook deliveries.
const DeliveryLoss = "Raw webhook deliveries are not replayed by reconcile: the tracker keeps no readable per-delivery history. The issue and comment state they report is recovered by reconcile as canonical observations; a raw delivery missed while push is unavailable is lost."

// Families declares every family a tracker's ingress emits.
func Families(typ string) []extension.EventFamily {
	t := TypesOf(typ)
	var out []extension.EventFamily
	for _, name := range []string{t.IssueOpen, t.IssueClosed, t.CommentHuman, t.CommentBot, t.CommentAgent} {
		out = append(out, extension.EventFamily{Type: name, ReDerivable: true})
	}
	return append(out, extension.EventFamily{Type: t.Delivery, Loss: DeliveryLoss})
}

// IssuePayload is the canonical observed issue state. It omits the update
// time, comment counts, projected status labels and a status the projector
// manages, so neither comments nor projection create issue observations.
// Lifecycle recurrences stay distinct through the reopen generation.
type IssuePayload struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Waiting bool   `json:"waiting,omitempty"`
	// Status is the workflow status, empty when it is one the entry's
	// transitions name (a projection).
	Status    string              `json:"status,omitempty"`
	Labels    []string            `json:"labels"`
	Attrs     map[string][]string `json:"attrs,omitempty"`
	Author    string              `json:"author,omitempty"`
	URL       string              `json:"url,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	// Generation is the reopen generation ("" before any reopening);
	// Predecessor is the previous generation's external ID.
	Generation  string `json:"generation,omitempty"`
	Predecessor string `json:"predecessor,omitempty"`
}

// CommentPayload is one comment on an issue.
type CommentPayload struct {
	Issue     string    `json:"issue"`
	ID        string    `json:"id"`
	Author    string    `json:"author,omitempty"`
	Bot       bool      `json:"bot,omitempty"`
	Body      string    `json:"body"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Edited    bool      `json:"edited,omitempty"`
	Agent     *Agent    `json:"agent,omitempty"`
}

// DeliveryPayload is a raw webhook delivery (one-shot).
type DeliveryPayload struct {
	Event    string   `json:"event"`
	Delivery string   `json:"delivery"`
	Issues   []string `json:"issues,omitempty"`
	Comments []string `json:"comments,omitempty"`
}

var keyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]{1,12}$`)

// ValidKey reports whether s looks like a tracker issue key.
func ValidKey(s string) bool { return keyPattern.MatchString(s) }

// IssueKey is the correlation external ID of an issue generation.
func IssueKey(key, generation string) string {
	if generation == "" {
		return "issue/" + key
	}
	return "issue/" + key + "/generation/" + generation
}

var issueKeyPattern = regexp.MustCompile(`^issue/([A-Za-z][A-Za-z0-9_]*-[0-9]{1,12})(?:/generation/(reopen-[0-9]+))?$`)

// ParseIssueKey extracts the issue key and generation from an external ID.
func ParseIssueKey(id string) (string, string, bool) {
	m := issueKeyPattern.FindStringSubmatch(id)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func generationOf(t time.Time) string { return "reopen-" + strconv.FormatInt(t.UTC().Unix(), 10) }

// generation is the reopen generation current at time at: the latest reopen
// at or before it. It depends only on immutable history, so push and
// reconcile agree. prev is the previous generation's external ID.
func generation(key string, reopens []time.Time, at time.Time) (gen, prev string, since time.Time) {
	sorted := append([]time.Time(nil), reopens...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	for _, r := range sorted {
		if r.After(at) {
			break
		}
		if gen == "" {
			prev = IssueKey(key, "")
		} else {
			prev = IssueKey(key, gen)
		}
		gen, since = generationOf(r), r.UTC()
	}
	return gen, prev, since
}

// normalizer turns Service reads into canonical observations. It is shared
// by push and reconcile so equal state has equal identity from either.
type normalizer struct {
	s        Settings
	svc      Service
	t        Types
	received time.Time
	reopens  map[string][]time.Time
}

func newNormalizer(s Settings, svc Service, received time.Time) *normalizer {
	return &normalizer{s: s, svc: svc, t: TypesOf(s.Type), received: received, reopens: map[string][]time.Time{}}
}

func (n *normalizer) history(ctx context.Context, key string) ([]time.Time, error) {
	if r, ok := n.reopens[key]; ok {
		return r, nil
	}
	r, err := n.svc.Reopens(ctx, key)
	if err != nil {
		return nil, err
	}
	n.reopens[key] = r
	return r, nil
}

func (n *normalizer) observation(item, typ string, key domain.CorrelationKey, occurred time.Time, schema string, payload any) (extension.Observation, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return extension.Observation{}, err
	}
	occurred = occurred.UTC()
	version := digest(typ, key.Source, key.ExternalID, occurred.Format(time.RFC3339Nano), schema, string(data))[:32]
	delivery := item + "@" + version
	k := key
	return extension.Observation{ID: item, Version: version, Event: domain.Event{
		ID: domain.ID(n.s.Type + "-" + digest(n.s.Namespace, delivery)[:40]), Type: typ, Source: n.s.Namespace, DeliveryID: delivery,
		OccurredAt: occurred, ReceivedAt: n.received, CorrelationKey: &k, Data: domain.Payload{Schema: schema, Data: data},
	}}, nil
}

func (n *normalizer) managedStatus(status string) bool {
	for _, s := range n.s.Transitions {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(status)) {
			return true
		}
	}
	return false
}

func (n *normalizer) labels(ls []string) []string {
	skip := map[string]bool{}
	for _, s := range n.s.Labels.Statuses() {
		skip[strings.ToLower(s)] = true
	}
	out := []string{}
	for _, l := range ls {
		if !skip[strings.ToLower(l)] {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func attrs(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string][]string{}
	for k, vs := range in {
		var keep []string
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" {
				keep = append(keep, v)
			}
		}
		if len(keep) > 0 {
			sort.Strings(keep)
			out[strings.ToLower(k)] = keep
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// issueObservation normalizes an issue in its current generation.
func (n *normalizer) issueObservation(ctx context.Context, i Issue) (extension.Observation, error) {
	if !ValidKey(i.Key) {
		return extension.Observation{}, errors.New("tracker issue without a valid key")
	}
	reopens, err := n.history(ctx, i.Key)
	if err != nil {
		return extension.Observation{}, err
	}
	gen, prev, since := generation(i.Key, reopens, time.Unix(1<<40, 0))
	state, typ := "open", n.t.IssueOpen
	if !i.Open {
		state, typ = "closed", n.t.IssueClosed
	}
	status := i.Status
	if n.managedStatus(status) {
		status = ""
	}
	p := IssuePayload{Key: i.Key, Title: i.Title, Body: i.Body, State: state, Waiting: i.Open && i.Waiting, Status: status,
		Labels: n.labels(i.Labels), Attrs: attrs(i.Attrs), Author: i.Author, URL: i.URL, CreatedAt: i.CreatedAt.UTC(),
		Generation: gen, Predecessor: prev}
	occurred := i.CreatedAt
	if since.After(occurred) {
		occurred = since
	}
	key := domain.CorrelationKey{Source: n.s.Namespace, ExternalID: IssueKey(i.Key, gen)}
	return n.observation("issue/"+i.Key, typ, key, occurred, n.t.SchemaIssue, p)
}

// commentObservation normalizes a comment, keyed to the issue generation
// current when it was written.
func (n *normalizer) commentObservation(ctx context.Context, issue string, c Comment) (extension.Observation, error) {
	if !ValidKey(issue) || c.ID == "" || strings.ContainsAny(c.ID, "/ \t\r\n") {
		return extension.Observation{}, errors.New("tracker comment without identity")
	}
	created, updated := c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	if created.IsZero() {
		created = updated
	}
	if updated.Before(created) {
		updated = created
	}
	if created.IsZero() {
		return extension.Observation{}, errors.New("tracker comment without timestamps")
	}
	reopens, err := n.history(ctx, issue)
	if err != nil {
		return extension.Observation{}, err
	}
	gen, _, _ := generation(issue, reopens, created)
	typ := n.t.CommentHuman
	switch {
	case c.Agent != nil:
		typ = n.t.CommentAgent
	case c.Bot:
		typ = n.t.CommentBot
	}
	p := CommentPayload{Issue: issue, ID: c.ID, Author: c.Author, Bot: c.Bot, Body: c.Body, URL: c.URL, CreatedAt: created,
		UpdatedAt: updated, Edited: updated.Sub(created) > 2*time.Second, Agent: c.Agent}
	key := domain.CorrelationKey{Source: n.s.Namespace, ExternalID: IssueKey(issue, gen)}
	return n.observation("issue/"+issue+"/comment/"+c.ID, typ, key, updated, n.t.SchemaComment, p)
}

// GenerationAt is the reopen generation current at time at, given an issue's
// reopen history ("" before any reopening). A code host uses it to key a
// pull request to the issue generation current when the PR was created.
func GenerationAt(reopens []time.Time, at time.Time) string {
	gen, _, _ := generation("", reopens, at)
	return gen
}
