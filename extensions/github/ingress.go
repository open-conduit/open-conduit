package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// DeliveryContentType marks a Delivery body produced by VerifyDelivery.
const DeliveryContentType = "application/vnd.open-conduit.github-delivery+json"

// envelope is the verified delivery handed to Push: GitHub's event name and
// delivery GUID travel in headers, so the webhook boundary wraps them.
type envelope struct {
	Event    string          `json:"event"`
	Delivery string          `json:"delivery"`
	Payload  json.RawMessage `json:"payload"`
}

type webhookPayload struct {
	Action     string `json:"action"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue       *rawIssue   `json:"issue"`
	Comment     *rawComment `json:"comment"`
	Review      *rawComment `json:"review"`
	PullRequest *rawPull    `json:"pull_request"`
	CheckSuite  *checkHook  `json:"check_suite"`
	CheckRun    *checkHook  `json:"check_run"`
	SHA         string      `json:"sha"`
	State       string      `json:"state"`
	Ref         string      `json:"ref"`
	RefType     string      `json:"ref_type"`
	Before      string      `json:"before"`
	After       string      `json:"after"`
	Commits     []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
}

type checkHook struct {
	HeadSHA      string `json:"head_sha"`
	Status       string `json:"status"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// IngressOptions tune scan pacing (zero values select defaults).
type IngressOptions struct {
	// BatchUnits bounds detail reads (events, PR details, parents) per page.
	BatchUnits int
	// PageBudget bounds wall time spent on one page before yielding a cursor.
	PageBudget time.Duration
	// MaxScanAge restarts a generation that has not completed in time.
	MaxScanAge time.Duration
	// MaxRepeats restarts a generation whose same page keeps being re-requested
	// after a successful answer (the runtime rejected it).
	MaxRepeats int
}

// Ingress is one repository's (push, reconcile) pair.
type Ingress struct {
	s   Settings
	c   *Client
	now func() time.Time
	o   IngressOptions

	mu       sync.Mutex
	scanMemo *memo
	memoID   string
	answered map[string]int
}

var _ extension.Ingress = (*Ingress)(nil)

// NewIngress binds an extension entry to a client.
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

// Settings returns the bound extension settings.
func (g *Ingress) Settings() Settings { return g.s }

// Spec declares every family; only raw deliveries are non-re-derivable.
func (g *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

func (g *Ingress) checkSource(src extension.Source) error {
	if src.Namespace != g.s.Namespace || string(src.ScopeID) != g.s.Scope {
		return extension.ErrInvalid
	}
	return nil
}

// Push normalizes one verified delivery. It performs read-only service calls
// (generation history, PR details, check state) and no coordination writes.
// The caller must have verified the signature (VerifyDelivery).
func (g *Ingress) Push(ctx context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out extension.PushResult
	if err := g.checkSource(src); err != nil {
		return out, err
	}
	if d.ContentType != DeliveryContentType {
		return out, extension.ErrInvalid
	}
	var env envelope
	if err := json.Unmarshal(d.Body, &env); err != nil || env.Event == "" || env.Delivery == "" || env.Delivery != d.ID {
		return out, extension.ErrInvalid
	}
	var p webhookPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return out, extension.ErrInvalid
	}
	if p.Repository == nil || !strings.EqualFold(p.Repository.FullName, g.s.Repo) || env.Event == "ping" {
		return out, nil
	}
	received := d.ReceivedAt.UTC()
	if received.IsZero() {
		received = g.now().UTC()
	}
	n := &normalizer{s: g.s, c: g.c, m: newMemo(), received: received}
	obs, err := g.pushObservations(ctx, n, env.Event, p)
	if err != nil {
		return extension.PushResult{}, err
	}
	out.Observations = obs
	out.Events = []domain.Event{g.deliveryEvent(env, p, received)}
	return out, nil
}

func (g *Ingress) pushObservations(ctx context.Context, n *normalizer, event string, p webhookPayload) ([]extension.Observation, error) {
	one := func(o extension.Observation, err error) ([]extension.Observation, error) {
		if errors.Is(err, errGone) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []extension.Observation{o}, nil
	}
	switch event {
	case "issues":
		if p.Issue == nil || p.Issue.PullRequest != nil || !g.s.TrackIssues || p.Action == "deleted" || p.Action == "transferred" {
			return nil, nil
		}
		return one(n.issueObservation(ctx, *p.Issue))
	case "issue_comment":
		if p.Issue == nil || p.Comment == nil || p.Action == "deleted" {
			return nil, nil
		}
		if p.Issue.PullRequest == nil && !g.s.TrackIssues {
			return nil, nil
		}
		return one(n.conversationComment(ctx, *p.Issue, *p.Comment))
	case "pull_request":
		if p.PullRequest == nil {
			return nil, nil
		}
		n.remember(*p.PullRequest)
		return one(n.pullObservation(ctx, *p.PullRequest))
	case "pull_request_review":
		if p.PullRequest == nil || p.Review == nil || p.Review.State == "PENDING" || strings.EqualFold(p.Review.State, "pending") {
			return nil, nil
		}
		n.remember(*p.PullRequest)
		key, _, err := n.pullKey(ctx, *p.PullRequest)
		if err != nil {
			return nil, err
		}
		review := *p.Review
		review.State = strings.ToUpper(review.State)
		return one(n.commentObservation(KindReview, p.PullRequest.Number, review, key))
	case "pull_request_review_comment":
		if p.PullRequest == nil || p.Comment == nil || p.Action == "deleted" {
			return nil, nil
		}
		n.remember(*p.PullRequest)
		return one(n.pullComment(ctx, KindReviewComment, p.PullRequest.Number, *p.Comment))
	case "check_run", "check_suite", "status":
		return g.pushChecks(ctx, n, event, p)
	}
	return nil, nil
}

// pushChecks re-reads the aggregate state for every open PR whose head is the
// delivery's commit, exactly as reconcile evaluates it.
func (g *Ingress) pushChecks(ctx context.Context, n *normalizer, event string, p webhookPayload) ([]extension.Observation, error) {
	var sha string
	var numbers []int
	switch event {
	case "check_run", "check_suite":
		hook := p.CheckRun
		if event == "check_suite" {
			hook = p.CheckSuite
		}
		if hook == nil || p.Action != "completed" || hook.HeadSHA == "" {
			return nil, nil
		}
		sha = hook.HeadSHA
		for _, pr := range hook.PullRequests {
			numbers = append(numbers, pr.Number)
		}
	case "status":
		if p.SHA == "" || p.State == "pending" {
			return nil, nil
		}
		sha = p.SHA
		pulls, err := g.c.pullsForCommit(ctx, g.s.Repo, sha)
		if err != nil {
			return nil, err
		}
		for _, pr := range pulls {
			numbers = append(numbers, pr.Number)
		}
	}
	sort.Ints(numbers)
	var out []extension.Observation
	seen := map[int]bool{}
	for _, number := range numbers {
		if seen[number] {
			continue
		}
		seen[number] = true
		pr, err := n.pull(ctx, number)
		if errors.Is(err, errGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if pr.State != "open" || pr.Head.SHA != sha {
			continue
		}
		o, ok, err := n.checksForPull(ctx, pr)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, o)
		}
	}
	return out, nil
}

func (g *Ingress) deliveryEvent(env envelope, p webhookPayload, received time.Time) domain.Event {
	ref := p.Ref
	if ref != "" && !strings.HasPrefix(ref, "refs/") {
		if p.RefType == "tag" {
			ref = "refs/tags/" + ref
		} else {
			ref = "refs/heads/" + ref
		}
	}
	files := []string{}
	seen := map[string]bool{}
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Removed, c.Modified} {
			for _, f := range list {
				if !seen[f] && len(files) < 1000 {
					seen[f] = true
					files = append(files, f)
				}
			}
		}
	}
	data, _ := json.Marshal(DeliveryPayload{Event: env.Event, Action: p.Action, Delivery: env.Delivery, Repo: g.s.Repo, Ref: ref, Before: p.Before, After: p.After, Files: files})
	delivery := "delivery/" + env.Delivery
	return domain.Event{ID: domain.ID("github-" + digest(g.s.Namespace, delivery)[:40]), Type: TypeDelivery, Source: g.s.Namespace,
		DeliveryID: delivery, OccurredAt: received, ReceivedAt: received, Data: domain.Payload{Schema: SchemaDelivery, Data: data}}
}

// ---- reconcile ------------------------------------------------------------

const (
	phaseIssues         = "issues"
	phasePulls          = "pulls"
	phaseComments       = "comments"
	phaseReviewComments = "review_comments"
	perPage             = 100
)

type cursor struct {
	phase        string
	page, offset int
}

func (c cursor) String() string { return fmt.Sprintf("%s:%d:%d", c.phase, c.page, c.offset) }

func parseCursor(s string) (cursor, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return cursor{}, false
	}
	page, err1 := strconv.Atoi(parts[1])
	offset, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || page < 1 || offset < 0 || offset >= perPage {
		return cursor{}, false
	}
	return cursor{parts[0], page, offset}, true
}

func (g *Ingress) phases() []string {
	if g.s.TrackIssues {
		return []string{phaseIssues, phasePulls, phaseComments, phaseReviewComments}
	}
	return []string{phasePulls, phaseComments, phaseReviewComments}
}

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

// Reconcile returns one bounded page of a generation. A generation lists every
// issue and PR in stable creation order (so pagination cannot repeat an item),
// observing open items and those closed within the horizon, then repository
// conversation and inline comments updated within the horizon. Older closed
// items and comments age out as generic absence, which carries no meaning.
// GitHub has no snapshot token: Complete certifies that every listing was
// read to its end within this generation; rate limits and failed reads return
// errors so no false absence is inferred.
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
		return extension.Snapshot{ScanID: scanID}, nil // foreign generation: restart
	}
	c := cursor{phase: g.phases()[0], page: 1}
	if scan.Cursor != "" {
		if c, ok = parseCursor(scan.Cursor); !ok {
			return extension.Snapshot{ScanID: scanID}, nil
		}
	}
	g.mu.Lock()
	if scan.ScanID == "" {
		// A new generation never reuses an earlier one's identity or reads.
		for scanID == g.memoID {
			start = start.Add(time.Nanosecond)
			scanID = "gen-" + strconv.FormatInt(start.UnixNano(), 10)
		}
		g.memoID = ""
	}
	answerKey := scanID + "|" + c.String()
	repeats := g.answered[answerKey]
	if g.memoID != scanID {
		g.memoID, g.scanMemo, g.answered = scanID, newMemo(), map[string]int{}
		repeats = 0
	}
	m := g.scanMemo
	g.mu.Unlock()
	if scan.ScanID != "" && (now.Sub(start) > g.o.MaxScanAge || repeats >= g.o.MaxRepeats) {
		// An exhausted partial page discards staging; the next scan starts afresh.
		return extension.Snapshot{ScanID: scanID}, nil
	}
	n := &normalizer{s: g.s, c: g.c, m: m, received: now}
	horizon := start.Add(-g.s.ClosedHorizon)
	snap, err := g.scanPage(ctx, n, c, horizon)
	if err != nil {
		return extension.Snapshot{}, err
	}
	snap.ScanID, snap.Checkpoint = scanID, scanID
	g.mu.Lock()
	if g.memoID == scanID {
		g.answered[answerKey]++
	}
	g.mu.Unlock()
	return snap, nil
}

func (g *Ingress) nextPhase(c cursor) (cursor, bool) {
	phases := g.phases()
	for i, p := range phases {
		if p == c.phase && i+1 < len(phases) {
			return cursor{phase: phases[i+1], page: 1}, true
		}
	}
	return cursor{}, false
}

type budget struct {
	started time.Time
	limit   time.Duration
	units   int
	max     int
}

func (b *budget) spent() bool { return b.units >= b.max || time.Since(b.started) >= b.limit }

func (g *Ingress) scanPage(ctx context.Context, n *normalizer, c cursor, horizon time.Time) (extension.Snapshot, error) {
	b := &budget{started: time.Now(), limit: g.o.PageBudget, max: g.o.BatchUnits}
	var obs []extension.Observation
	var count int
	var more bool
	var next int
	var err error
	switch c.phase {
	case phaseIssues:
		obs, count, more, next, err = g.issuesPage(ctx, n, c, horizon, b)
	case phasePulls:
		obs, count, more, next, err = g.pullsPage(ctx, n, c, horizon, b)
	case phaseComments, phaseReviewComments:
		obs, count, more, next, err = g.commentsPage(ctx, n, c, horizon, b)
	default:
		return extension.Snapshot{}, extension.ErrInvalid
	}
	if err != nil {
		return extension.Snapshot{}, err
	}
	snap := extension.Snapshot{Observations: dedupe(obs)}
	switch {
	case next < count:
		snap.NextCursor = cursor{c.phase, c.page, next}.String()
	case more:
		snap.NextCursor = cursor{c.phase, c.page + 1, 0}.String()
	default:
		if nc, ok := g.nextPhase(c); ok {
			snap.NextCursor = nc.String()
		} else {
			snap.Complete = true
		}
	}
	return snap, nil
}

// dedupe drops exact repeats within one page (same item and version).
func dedupe(obs []extension.Observation) []extension.Observation {
	seen := map[string]bool{}
	out := []extension.Observation{}
	for _, o := range obs {
		if !seen[o.ID+"@"+o.Version] {
			seen[o.ID+"@"+o.Version] = true
			out = append(out, o)
		}
	}
	return out
}

func recent(state string, updated time.Time, horizon time.Time) bool {
	return state == "open" || !updated.Before(horizon)
}

func (g *Ingress) issuesPage(ctx context.Context, n *normalizer, c cursor, horizon time.Time, b *budget) ([]extension.Observation, int, bool, int, error) {
	items, more, err := page[rawIssue](ctx, g.c, g.s.Repo, repoPath(g.s.Repo, "/issues?state=all&sort=created&direction=asc"), c.page, perPage)
	if err != nil {
		return nil, 0, false, 0, err
	}
	var out []extension.Observation
	i := c.offset
	for ; i < len(items); i++ {
		if i > c.offset && b.spent() {
			break
		}
		it := items[i]
		if it.PullRequest != nil || !recent(it.State, it.UpdatedAt, horizon) {
			continue
		}
		if it.State == "closed" || strings.EqualFold(it.StateReason, "reopened") {
			b.units++
		}
		o, err := n.issueObservation(ctx, it)
		if err != nil {
			return nil, 0, false, 0, err
		}
		out = append(out, o)
	}
	return out, len(items), more, i, nil
}

func (g *Ingress) pullsPage(ctx context.Context, n *normalizer, c cursor, horizon time.Time, b *budget) ([]extension.Observation, int, bool, int, error) {
	items, more, err := page[rawPull](ctx, g.c, g.s.Repo, repoPath(g.s.Repo, "/pulls?state=all&sort=created&direction=asc"), c.page, perPage)
	if err != nil {
		return nil, 0, false, 0, err
	}
	var out []extension.Observation
	i := c.offset
	for ; i < len(items); i++ {
		if i > c.offset && b.spent() {
			break
		}
		p := items[i]
		if !recent(p.State, p.UpdatedAt, horizon) {
			continue
		}
		b.units += 3
		n.remember(p)
		o, err := n.pullObservation(ctx, p)
		if err != nil {
			return nil, 0, false, 0, err
		}
		out = append(out, o)
		reviews, err := n.reviewObservations(ctx, p)
		if err != nil {
			return nil, 0, false, 0, err
		}
		out = append(out, reviews...)
		checks, ok, err := n.checksForPull(ctx, p)
		if err != nil {
			return nil, 0, false, 0, err
		}
		if ok {
			out = append(out, checks)
		}
	}
	return out, len(items), more, i, nil
}

func (g *Ingress) commentsPage(ctx context.Context, n *normalizer, c cursor, horizon time.Time, b *budget) ([]extension.Observation, int, bool, int, error) {
	path := "/issues/comments"
	if c.phase == phaseReviewComments {
		path = "/pulls/comments"
	}
	items, more, err := page[rawComment](ctx, g.c, g.s.Repo, repoPath(g.s.Repo, path+"?sort=created&direction=asc&since=%s", url.QueryEscape(horizon.Format(time.RFC3339))), c.page, perPage)
	if err != nil {
		return nil, 0, false, 0, err
	}
	var out []extension.Observation
	i := c.offset
	for ; i < len(items); i++ {
		if i > c.offset && b.spent() {
			break
		}
		cm := items[i]
		var o extension.Observation
		if c.phase == phaseReviewComments {
			number, ok := numberFromURL(cm.PullRequestURL)
			if !ok {
				continue
			}
			b.units++
			o, err = n.pullComment(ctx, KindReviewComment, number, cm)
		} else {
			number, ok := numberFromURL(cm.IssueURL)
			if !ok {
				continue
			}
			b.units++
			var parent rawIssue
			parent, err = n.issue(ctx, g.s.Repo, number)
			if err == nil {
				if parent.PullRequest == nil && !g.s.TrackIssues {
					continue
				}
				o, err = n.conversationComment(ctx, parent, cm)
			}
		}
		if errors.Is(err, errGone) {
			continue
		}
		if err != nil {
			return nil, 0, false, 0, err
		}
		out = append(out, o)
	}
	return out, len(items), more, i, nil
}
