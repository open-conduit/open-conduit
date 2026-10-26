package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/extensions/tracker"
)

// Event families. Types name observed state, not webhook actions.
const (
	TypePullOpen      = "bitbucket.pull_request.open"
	TypePullClosed    = "bitbucket.pull_request.closed"
	TypePullMerged    = "bitbucket.pull_request.merged"
	TypeCommentHuman  = "bitbucket.comment.human"
	TypeCommentBot    = "bitbucket.comment.bot"
	TypeCommentAgent  = "bitbucket.comment.agent"
	TypeChecksPending = "bitbucket.checks.pending"
	TypeChecksPassed  = "bitbucket.checks.passed"
	TypeChecksFailed  = "bitbucket.checks.failed"
	TypeDelivery      = "bitbucket.delivery"
)

// Payload schemas.
const (
	SchemaPull     = "bitbucket.pull_request.v1"
	SchemaComment  = "bitbucket.comment.v1"
	SchemaChecks   = "bitbucket.checks.v1"
	SchemaDelivery = "bitbucket.delivery.v1"
)

// DeliveryContentType marks a Delivery body produced by VerifyDelivery.
const DeliveryContentType = "application/vnd.open-conduit.bitbucket-delivery+json"

// Families declares every emitted family; only raw deliveries are one-shot.
func Families() []extension.EventFamily {
	var out []extension.EventFamily
	for _, t := range []string{TypePullOpen, TypePullClosed, TypePullMerged, TypeCommentHuman, TypeCommentBot, TypeCommentAgent, TypeChecksPending, TypeChecksPassed, TypeChecksFailed} {
		out = append(out, extension.EventFamily{Type: t, ReDerivable: true})
	}
	return append(out, extension.EventFamily{Type: TypeDelivery, Loss: tracker.DeliveryLoss})
}

// PullPayload is the canonical observed PR state. It omits the update time,
// so comments never create PR observations.
type PullPayload struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	HeadRef   string    `json:"head_ref"`
	HeadSHA   string    `json:"head_sha"`
	BaseRef   string    `json:"base_ref"`
	Author    string    `json:"author,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Issue is the tracker issue key the PR belongs to ("" for none).
	Issue string `json:"issue,omitempty"`
}

// CommentPayload is one PR comment (conversation or inline).
type CommentPayload struct {
	Repo      string         `json:"repo"`
	Number    int            `json:"number"`
	ID        int            `json:"id"`
	Author    string         `json:"author,omitempty"`
	Bot       bool           `json:"bot,omitempty"`
	Body      string         `json:"body"`
	URL       string         `json:"url,omitempty"`
	Path      string         `json:"path,omitempty"`
	Line      int            `json:"line,omitempty"`
	InReplyTo int            `json:"in_reply_to,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Edited    bool           `json:"edited,omitempty"`
	Agent     *tracker.Agent `json:"agent,omitempty"`
}

// CheckRun is one commit status.
type CheckRun struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// ChecksPayload is a PR head's combined commit status.
type ChecksPayload struct {
	Repo    string     `json:"repo"`
	Number  int        `json:"number"`
	SHA     string     `json:"sha"`
	State   string     `json:"state"`
	Runs    []CheckRun `json:"runs"`
	Failed  []string   `json:"failed,omitempty"`
	Preview string     `json:"preview,omitempty"`
}

// DeliveryPayload is a raw webhook delivery (one-shot).
type DeliveryPayload struct {
	Event    string `json:"event"`
	Delivery string `json:"delivery"`
	Number   int    `json:"number,omitempty"`
}

// PullKey is the correlation external ID of a PR no tracker issue claims.
func PullKey(number int) string { return "pr/" + strconv.Itoa(number) }

var pullKeyPattern = regexp.MustCompile(`^pr/([0-9]{1,12})$`)

// ParsePullKey extracts a PR number.
func ParsePullKey(id string) (int, bool) {
	m := pullKeyPattern.FindStringSubmatch(id)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// Generations resolves the tracker issue generation current at a time (the
// tracker's reopen history), so a PR correlates with the issue's live Work.
// Nil, or an error, keys the base generation.
type Generations func(ctx context.Context, key string, at time.Time) (string, error)

// envelope is the verified delivery handed to Push.
type envelope struct {
	ID      string `json:"id"`
	Event   string `json:"event"`
	PR      int    `json:"pr,omitempty"`
	Comment int    `json:"comment,omitempty"`
	SHA     string `json:"sha,omitempty"`
}

var (
	deliveryID = regexp.MustCompile(`^[A-Za-z0-9{}-]{1,128}$`)
	shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// VerifyDelivery authenticates a raw webhook request (X-Hub-Signature,
// HMAC-SHA256) and wraps what it reports into the Delivery Push accepts.
func VerifyDelivery(s Settings, secret string, h http.Header, body []byte, received time.Time) (extension.Delivery, error) {
	sig := h.Get("X-Hub-Signature")
	if sig == "" {
		sig = h.Get("X-Hub-Signature-256")
	}
	if !tracker.VerifyHMAC(secret, body, sig) {
		return extension.Delivery{}, tracker.ErrSignature
	}
	var p struct {
		PullRequest *struct {
			ID int `json:"id"`
		} `json:"pullrequest"`
		Comment *struct {
			ID int `json:"id"`
		} `json:"comment"`
		CommitStatus *struct {
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
		} `json:"commit_status"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return extension.Delivery{}, extension.ErrInvalid
	}
	env := envelope{Event: h.Get("X-Event-Key")}
	if id := h.Get("X-Request-UUID"); deliveryID.MatchString(id) {
		env.ID = strings.Trim(id, "{}")
	} else {
		env.ID = tracker.Digest(string(body))[:32]
	}
	if p.Repository.FullName == "" || strings.EqualFold(p.Repository.FullName, s.Repo) {
		if p.PullRequest != nil {
			env.PR = p.PullRequest.ID
		}
		if p.Comment != nil {
			env.Comment = p.Comment.ID
		}
		if p.CommitStatus != nil && shaPattern.MatchString(p.CommitStatus.Commit.Hash) {
			env.SHA = p.CommitStatus.Commit.Hash
		}
	}
	wrapped, _ := json.Marshal(env)
	return extension.Delivery{ID: env.ID, ReceivedAt: received.UTC(), ContentType: DeliveryContentType, Body: wrapped}, nil
}

// IngressOptions tune scan pacing.
type IngressOptions struct {
	BatchUnits  int
	PageBudget  time.Duration
	MaxScanAge  time.Duration
	MaxRepeats  int
	Generations Generations
}

// Ingress is one repository's (push, reconcile) pair.
type Ingress struct {
	s   Settings
	c   *Client
	now func() time.Time
	o   IngressOptions

	mu       sync.Mutex
	answered map[string]int
	memoID   string
}

var _ extension.Ingress = (*Ingress)(nil)

// NewIngress binds an entry to a client.
func NewIngress(s Settings, c *Client, now func() time.Time, o IngressOptions) *Ingress {
	if now == nil {
		now = time.Now
	}
	if o.BatchUnits <= 0 {
		o.BatchUnits = 25
	}
	if o.PageBudget <= 0 {
		o.PageBudget = 10 * time.Second
	}
	if o.MaxScanAge <= 0 {
		o.MaxScanAge = 6 * time.Hour
	}
	if o.MaxRepeats <= 0 {
		o.MaxRepeats = 3
	}
	return &Ingress{s: s, c: c, now: now, o: o, answered: map[string]int{}}
}

// Settings returns the bound settings.
func (g *Ingress) Settings() Settings { return g.s }

// Spec declares every family.
func (g *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

func (g *Ingress) checkSource(src extension.Source) error {
	if src.Namespace != g.s.Namespace || string(src.ScopeID) != g.s.Scope {
		return extension.ErrInvalid
	}
	return nil
}

type normalizer struct {
	s        Settings
	gens     Generations
	received time.Time
}

func digest(parts ...string) string { return tracker.Digest(parts...) }

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
		ID: domain.ID("bitbucket-" + digest(n.s.Namespace, delivery)[:40]), Type: typ, Source: n.s.Namespace, DeliveryID: delivery,
		OccurredAt: occurred, ReceivedAt: n.received, CorrelationKey: &k, Data: domain.Payload{Schema: schema, Data: data},
	}}, nil
}

// pullKey derives a PR's correlation: its tracker issue's generation at the
// PR's creation, else its own key.
func (n *normalizer) pullKey(ctx context.Context, p rawPR) (domain.CorrelationKey, string, error) {
	issue := n.s.IssueOf(p.Source.Branch.Name, p.Title, p.Description)
	if issue == "" {
		return domain.CorrelationKey{Source: n.s.Namespace, ExternalID: PullKey(p.ID)}, "", nil
	}
	gen := ""
	if n.gens != nil {
		g, err := n.gens(ctx, issue, parseTime(p.CreatedOn))
		if err != nil && !errors.Is(err, tracker.ErrNotFound) {
			return domain.CorrelationKey{}, "", err
		}
		gen = g
	}
	return domain.CorrelationKey{Source: n.s.TrackerNS, ExternalID: tracker.IssueKey(issue, gen)}, issue, nil
}

func pullState(p rawPR) (string, string) {
	switch p.State {
	case "MERGED":
		return "merged", TypePullMerged
	case "DECLINED", "SUPERSEDED":
		return strings.ToLower(p.State), TypePullClosed
	}
	return "open", TypePullOpen
}

func (n *normalizer) pull(ctx context.Context, p rawPR) (extension.Observation, domain.CorrelationKey, error) {
	key, issue, err := n.pullKey(ctx, p)
	if err != nil {
		return extension.Observation{}, key, err
	}
	state, typ := pullState(p)
	created := parseTime(p.CreatedOn)
	payload := PullPayload{Repo: n.s.Repo, Number: p.ID, Title: p.Title, Body: p.Description, State: state, HeadRef: p.Source.Branch.Name,
		HeadSHA: p.Source.Commit.Hash, BaseRef: p.Destination.Branch.Name, Author: p.Author.name(), URL: p.Links.HTML.Href, CreatedAt: created, Issue: issue}
	o, err := n.observation(PullKey(p.ID), typ, key, created, SchemaPull, payload)
	return o, key, err
}

func (n *normalizer) comment(pr int, c rawComment, key domain.CorrelationKey) (extension.Observation, error) {
	body, agent := Unmark(c.Content.Raw)
	typ := TypeCommentHuman
	switch {
	case agent != nil:
		typ = TypeCommentAgent
	case c.User.Type == "app_user":
		typ = TypeCommentBot
	}
	created, updated := parseTime(c.CreatedOn), parseTime(c.UpdatedOn)
	if updated.Before(created) {
		updated = created
	}
	if created.IsZero() {
		return extension.Observation{}, errors.New("bitbucket comment without timestamps")
	}
	p := CommentPayload{Repo: n.s.Repo, Number: pr, ID: c.ID, Author: c.User.name(), Bot: c.User.Type == "app_user", Body: body, URL: c.Links.HTML.Href,
		CreatedAt: created, UpdatedAt: updated, Edited: updated.Sub(created) > 2*time.Second, Agent: agent}
	if c.Inline != nil {
		p.Path = c.Inline.Path
		if c.Inline.To != nil {
			p.Line = *c.Inline.To
		} else if c.Inline.From != nil {
			p.Line = *c.Inline.From
		}
	}
	if c.Parent != nil {
		p.InReplyTo = c.Parent.ID
	}
	return n.observation(PullKey(pr)+"/comment/"+strconv.Itoa(c.ID), typ, key, updated, SchemaComment, p)
}

// checks evaluates the head's statuses; ok is false when none reported.
func (n *normalizer) checks(p rawPR, statuses []rawStatus, key domain.CorrelationKey) (extension.Observation, bool, error) {
	if len(statuses) == 0 || p.Source.Commit.Hash == "" {
		return extension.Observation{}, false, nil
	}
	payload := ChecksPayload{Repo: n.s.Repo, Number: p.ID, SHA: p.Source.Commit.Hash, Runs: []CheckRun{}}
	pending := false
	var latest time.Time
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Key < statuses[j].Key })
	for _, s := range statuses {
		name := s.Name
		if name == "" {
			name = s.Key
		}
		run := CheckRun{Name: name, URL: s.URL}
		switch s.State {
		case "SUCCESSFUL":
			run.State = "success"
			if payload.Preview == "" && s.URL != "" && n.s.Preview != nil && (n.s.Preview.MatchString(s.Key) || n.s.Preview.MatchString(s.Name)) {
				payload.Preview = s.URL
			}
		case "FAILED", "STOPPED":
			run.State = "failure"
			payload.Failed = append(payload.Failed, name)
		default:
			run.State = "pending"
			pending = true
		}
		payload.Runs = append(payload.Runs, run)
		if t := parseTime(s.UpdatedOn); t.After(latest) {
			latest = t
		}
	}
	typ := TypeChecksPassed
	switch {
	case pending:
		payload.State, typ = "pending", TypeChecksPending
	case len(payload.Failed) > 0:
		payload.State, typ = "failed", TypeChecksFailed
	default:
		payload.State = "passed"
	}
	if latest.IsZero() {
		latest = parseTime(p.CreatedOn)
	}
	o, err := n.observation(PullKey(p.ID)+"/checks", typ, key, latest, SchemaChecks, payload)
	return o, err == nil, err
}

// Push normalizes one verified delivery by reading the PR, comment or
// statuses it reports back from the API, so push yields the same
// observations reconcile would, plus the one-shot raw delivery Event.
func (g *Ingress) Push(ctx context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out extension.PushResult
	if err := g.checkSource(src); err != nil {
		return out, err
	}
	if d.ContentType != DeliveryContentType {
		return out, extension.ErrInvalid
	}
	var env envelope
	if err := json.Unmarshal(d.Body, &env); err != nil || env.ID == "" || env.ID != d.ID {
		return out, extension.ErrInvalid
	}
	received := d.ReceivedAt.UTC()
	if received.IsZero() {
		received = g.now().UTC()
	}
	n := &normalizer{s: g.s, gens: g.o.Generations, received: received}
	gone := func(err error) bool { return errors.Is(err, tracker.ErrNotFound) }
	switch {
	case env.PR > 0 && env.Comment > 0:
		p, err := g.c.Pull(ctx, g.s, env.PR)
		if err != nil && !gone(err) {
			return extension.PushResult{}, err
		}
		if err == nil {
			key, _, err := n.pullKey(ctx, p)
			if err != nil {
				return extension.PushResult{}, err
			}
			c, err := g.c.Comment(ctx, g.s, env.PR, env.Comment)
			if err != nil && !gone(err) {
				return extension.PushResult{}, err
			}
			if err == nil && !c.Deleted {
				o, err := n.comment(env.PR, c, key)
				if err != nil {
					return extension.PushResult{}, err
				}
				out.Observations = append(out.Observations, o)
			}
		}
	case env.PR > 0:
		p, err := g.c.Pull(ctx, g.s, env.PR)
		if err != nil && !gone(err) {
			return extension.PushResult{}, err
		}
		if err == nil {
			o, _, err := n.pull(ctx, p)
			if err != nil {
				return extension.PushResult{}, err
			}
			out.Observations = append(out.Observations, o)
		}
	case env.SHA != "":
		prs, err := g.c.OpenPullsAt(ctx, g.s, env.SHA)
		if err != nil {
			return extension.PushResult{}, err
		}
		sort.Slice(prs, func(i, j int) bool { return prs[i].ID < prs[j].ID })
		for _, p := range prs {
			key, _, err := n.pullKey(ctx, p)
			if err != nil {
				return extension.PushResult{}, err
			}
			statuses, err := g.c.Statuses(ctx, g.s, p.Source.Commit.Hash)
			if err != nil {
				return extension.PushResult{}, err
			}
			if o, ok, err := n.checks(p, statuses, key); err != nil {
				return extension.PushResult{}, err
			} else if ok {
				out.Observations = append(out.Observations, o)
			}
		}
	}
	data, _ := json.Marshal(DeliveryPayload{Event: env.Event, Delivery: env.ID, Number: env.PR})
	delivery := "delivery/" + env.ID
	out.Events = []domain.Event{{ID: domain.ID("bitbucket-" + digest(g.s.Namespace, delivery)[:40]), Type: TypeDelivery, Source: g.s.Namespace,
		DeliveryID: delivery, OccurredAt: received, ReceivedAt: received, Data: domain.Payload{Schema: SchemaDelivery, Data: data}}}
	return out, nil
}

// ---- reconcile ----------------------------------------------------------------

func scanStart(id string) (time.Time, bool) {
	v, ok := strings.CutPrefix(id, "gen-")
	if !ok {
		return time.Time{}, false
	}
	nanos, err := strconv.ParseInt(v, 10, 64)
	if err != nil || nanos <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, nanos).UTC(), true
}

// cursor is "<offset>|<next link>" within the PR listing.
func splitCursor(s string) (int, string, bool) {
	off, link, ok := strings.Cut(s, "|")
	if !ok {
		return 0, "", false
	}
	n, err := strconv.Atoi(off)
	if err != nil || n < 0 {
		return 0, "", false
	}
	return n, link, true
}

// Reconcile returns one bounded page of a generation: the repository's PRs
// in creation order (open ones and those updated within the horizon), each
// with its comments and its head's statuses. Complete certifies the listing
// was read to its end; failures return errors so no absence is inferred.
func (g *Ingress) Reconcile(ctx context.Context, scan extension.Scan) (extension.Snapshot, error) {
	if err := g.checkSource(scan.Source); err != nil {
		return extension.Snapshot{}, err
	}
	now := g.now().UTC()
	scanID := scan.ScanID
	start, ok := scanStart(scanID)
	if scanID == "" {
		start, scanID = now, "gen-"+strconv.FormatInt(now.UnixNano(), 10)
	} else if !ok {
		return extension.Snapshot{ScanID: scanID}, nil
	}
	offset, link := 0, ""
	if scan.Cursor != "" {
		if offset, link, ok = splitCursor(scan.Cursor); !ok {
			return extension.Snapshot{ScanID: scanID}, nil
		}
	}
	g.mu.Lock()
	if scan.ScanID == "" {
		for scanID == g.memoID {
			start = start.Add(time.Nanosecond)
			scanID = "gen-" + strconv.FormatInt(start.UnixNano(), 10)
		}
		g.memoID = ""
	}
	answerKey := scanID + "|" + scan.Cursor
	repeats := g.answered[answerKey]
	if g.memoID != scanID {
		g.memoID, g.answered = scanID, map[string]int{}
		repeats = 0
	}
	g.mu.Unlock()
	if scan.ScanID != "" && (now.Sub(start) > g.o.MaxScanAge || repeats >= g.o.MaxRepeats) {
		return extension.Snapshot{ScanID: scanID}, nil
	}
	prs, next, err := g.c.ListPulls(ctx, g.s, start.Add(-g.s.ClosedHorizon), link)
	if err != nil {
		return extension.Snapshot{}, err
	}
	n := &normalizer{s: g.s, gens: g.o.Generations, received: now}
	snap := extension.Snapshot{ScanID: scanID, Checkpoint: scanID}
	deadline := time.Now().Add(g.o.PageBudget)
	units := 0
	i := offset
	for ; i < len(prs); i++ {
		if units >= g.o.BatchUnits || (units > 0 && time.Now().After(deadline)) {
			break
		}
		p := prs[i]
		o, key, err := n.pull(ctx, p)
		if err != nil {
			return extension.Snapshot{}, err
		}
		snap.Observations = append(snap.Observations, o)
		comments, err := g.c.Comments(ctx, g.s, p.ID)
		units++
		if err != nil {
			return extension.Snapshot{}, err
		}
		for _, c := range comments {
			if o, err := n.comment(p.ID, c, key); err == nil {
				snap.Observations = append(snap.Observations, o)
			}
		}
		if p.Source.Commit.Hash != "" {
			statuses, err := g.c.Statuses(ctx, g.s, p.Source.Commit.Hash)
			units++
			if err != nil && !errors.Is(err, tracker.ErrNotFound) {
				return extension.Snapshot{}, err
			}
			if o, ok, err := n.checks(p, statuses, key); err != nil {
				return extension.Snapshot{}, err
			} else if ok {
				snap.Observations = append(snap.Observations, o)
			}
		}
	}
	switch {
	case i < len(prs):
		snap.NextCursor = strconv.Itoa(i) + "|" + link
	case next != "":
		snap.NextCursor = "0|" + next
	default:
		snap.Complete = true
	}
	g.mu.Lock()
	if g.memoID == scanID {
		g.answered[answerKey]++
	}
	g.mu.Unlock()
	return snap, nil
}
