package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
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
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	// CheckSuite is a check run's suite, which names the run's branch.
	CheckSuite *struct {
		HeadBranch string `json:"head_branch"`
	} `json:"check_suite"`
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
	// Observed, when set, sees each push's and scan page's observations
	// before the host accepts them (wait progress). It cannot fail them.
	Observed func(ctx context.Context, obs []extension.Observation)
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
	// branches remembers whether a commit is on a branch (checkRef).
	branches map[string]bool
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
	return &Ingress{s: s, c: c, now: now, o: o, answered: map[string]int{}, branches: map[string]bool{}}
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
	if err == nil {
		obs, err = n.withCopies(obs)
	}
	if err != nil {
		return extension.PushResult{}, err
	}
	if g.o.Observed != nil {
		g.o.Observed(ctx, obs)
	}
	out.Observations = obs
	out.Events = []domain.Event{g.deliveryEvent(env, p, g.checkRef(ctx, env.Event, p), received)}
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
		obs, err := one(n.pullObservation(ctx, *p.PullRequest))
		if err != nil {
			return nil, err
		}
		// The delivery carries mergeable when GitHub had already worked it
		// out; reconcile reads it otherwise.
		c, ok, err := n.conflictsObservation(ctx, *p.PullRequest)
		if err != nil {
			return nil, err
		}
		if ok {
			obs = append(obs, c)
		}
		return obs, nil
	case "pull_request_review":
		if p.PullRequest == nil || p.Review == nil || p.Review.State == "PENDING" || strings.EqualFold(p.Review.State, "pending") || strings.TrimSpace(p.Review.Body) == "" {
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

// checkHead is a finished check suite's or run's hook and the head branch
// GitHub names for it; nil for other events.
func checkHead(event string, p webhookPayload) (*checkHook, string) {
	hook := p.CheckRun
	if event == "check_suite" {
		hook = p.CheckSuite
	} else if event != "check_run" {
		return nil, ""
	}
	if hook == nil {
		return nil, ""
	}
	branch := hook.HeadBranch
	if branch == "" && hook.CheckSuite != nil {
		branch = hook.CheckSuite.HeadBranch
	}
	return hook, branch
}

// maxBranchChecks bounds the remembered answers of checkRef.
const maxBranchChecks = 512

// checkRef is the ref a check suite or run delivery names: its head branch,
// but only when the commit is in that branch's history in this repository.
// GitHub names the head repository's branch, so a fork PR from the fork's
// own main reports head_branch main for a commit main doesn't have. When
// GitHub can't be asked, it names no ref (fail closed), so no branch-filtered
// job runs on it. Answers are remembered per branch and commit, since one
// commit's suites and runs arrive together.
func (g *Ingress) checkRef(ctx context.Context, event string, p webhookPayload) string {
	hook, branch := checkHead(event, p)
	if hook == nil || branch == "" || hook.HeadSHA == "" {
		return ""
	}
	key := branch + "\x00" + hook.HeadSHA
	g.mu.Lock()
	on, known := g.branches[key]
	g.mu.Unlock()
	if !known {
		var err error
		if on, err = g.c.onBranch(ctx, g.s.Repo, branch, hook.HeadSHA); err != nil {
			return ""
		}
		g.mu.Lock()
		if len(g.branches) >= maxBranchChecks {
			g.branches = map[string]bool{}
		}
		g.branches[key] = on
		g.mu.Unlock()
	}
	if !on {
		return ""
	}
	return "refs/heads/" + branch
}

func (g *Ingress) deliveryEvent(env envelope, p webhookPayload, checkRef string, received time.Time) domain.Event {
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
				if !seen[f] && len(files) < MaxDeliveryFiles {
					seen[f] = true
					files = append(files, f)
				}
			}
		}
	}
	after, conclusion := p.After, ""
	// A finished check suite or run names its commit, branch and
	// conclusion, so a job can run when a branch's checks fail.
	if hook, _ := checkHead(env.Event, p); hook != nil {
		ref, after, conclusion = checkRef, hook.HeadSHA, hook.Conclusion
	}
	data, _ := json.Marshal(DeliveryPayload{Event: env.Event, Action: p.Action, Delivery: env.Delivery, Repo: g.s.Repo, Ref: ref, Before: p.Before, After: after, Conclusion: conclusion, Files: files, Commits: len(p.Commits)})
	delivery := "delivery/" + env.Delivery
	return domain.Event{ID: domain.ID("github-" + digest(g.s.Namespace, delivery)[:40]), Type: TypeDelivery, Source: g.s.Namespace,
		DeliveryID: delivery, OccurredAt: received, ReceivedAt: received, Data: domain.Payload{Schema: SchemaDelivery, Data: data}}
}

// ---- reconcile ------------------------------------------------------------

const (
	phaseOpenIssues     = "open_issues"
	phaseClosedIssues   = "closed_issues"
	phaseOpenPulls      = "open_pulls"
	phaseClosedPulls    = "closed_pulls"
	phaseComments       = "comments"
	phaseReviewComments = "review_comments"
	perPage             = 100
	// maxLookback bounds how far back a generation looks after a long
	// stretch without a completed one.
	maxLookback = 168 * time.Hour
)

type cursor struct {
	phase        string
	page, offset int
}

func (c cursor) String() string { return fmt.Sprintf("%s:%d:%d", c.phase, c.page, c.offset) }

func (g *Ingress) parseCursor(s string) (cursor, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 || !slices.Contains(g.phases(), parts[0]) {
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
		return []string{phaseOpenIssues, phaseClosedIssues, phaseOpenPulls, phaseClosedPulls, phaseComments, phaseReviewComments}
	}
	return []string{phaseOpenPulls, phaseClosedPulls, phaseComments, phaseReviewComments}
}

// A generation's ID is gen-<start> or gen-<start>-<horizon> (Unix
// nanoseconds), so every page of it, in any process, reads the same window.
// An ID without a horizon is an earlier version's generation.
func scanID(start, horizon time.Time) string {
	return "gen-" + strconv.FormatInt(start.UnixNano(), 10) + "-" + strconv.FormatInt(horizon.UnixNano(), 10)
}

func parseScanID(id string) (start, horizon time.Time, ok bool) {
	v, ok := strings.CutPrefix(id, "gen-")
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	startPart, horizonPart, both := strings.Cut(v, "-")
	nanos, err := strconv.ParseInt(startPart, 10, 64)
	if err != nil || nanos <= 0 {
		return time.Time{}, time.Time{}, false
	}
	start = time.Unix(0, nanos).UTC()
	if !both {
		return start, time.Time{}, true
	}
	h, err := strconv.ParseInt(horizonPart, 10, 64)
	if err != nil || h <= 0 || h > nanos {
		return time.Time{}, time.Time{}, false
	}
	return start, time.Unix(0, h).UTC(), true
}

// horizon is how far back a generation starting at start looks for closed
// items and comments: ClosedHorizon before it, or, when the last completed
// generation (checkpoint) began before that, back to that generation's
// start, so a stretch of failed scans loses nothing that closed meanwhile.
// It never reaches back further than maxLookback.
func (g *Ingress) horizon(start time.Time, checkpoint string) time.Time {
	h := start.Add(-g.s.ClosedHorizon)
	if last, _, ok := parseScanID(checkpoint); ok && last.Before(h) {
		h = last
	}
	if floor := start.Add(-maxLookback); h.Before(floor) {
		h = floor
	}
	return h
}

// Reconcile returns one bounded page of a generation. A generation lists the
// open issues and PRs in creation order, then the closed ones most recently
// updated first, stopping at the first updated before the horizon and
// observing those that closed within it, then repository conversation and
// inline comments updated within the horizon. So its cost follows the
// repository's open items and recent activity, not its history. Older closed
// items and comments age out as generic absence, which carries no meaning.
// GitHub has no snapshot token: Complete certifies that every listing was
// read to its end within this generation; rate limits and failed reads return
// errors so no false absence is inferred.
func (g *Ingress) Reconcile(ctx context.Context, scan extension.Scan) (extension.Snapshot, error) {
	if err := g.checkSource(scan.Source); err != nil {
		return extension.Snapshot{}, err
	}
	now := g.now().UTC()
	id := scan.ScanID
	start, horizon, ok := parseScanID(id)
	if id == "" {
		start = now
		horizon = g.horizon(start, scan.Checkpoint)
		id = scanID(start, horizon)
	} else if !ok || horizon.IsZero() {
		return extension.Snapshot{ScanID: id}, nil // foreign or earlier generation: restart
	}
	c := cursor{phase: g.phases()[0], page: 1}
	if scan.Cursor != "" {
		if c, ok = g.parseCursor(scan.Cursor); !ok {
			return extension.Snapshot{ScanID: id}, nil
		}
	}
	g.mu.Lock()
	if scan.ScanID == "" {
		// A new generation never reuses an earlier one's identity or reads.
		for id == g.memoID {
			start = start.Add(time.Nanosecond)
			id = scanID(start, horizon)
		}
		g.memoID = ""
	}
	answerKey := id + "|" + c.String()
	repeats := g.answered[answerKey]
	if g.memoID != id {
		g.memoID, g.scanMemo, g.answered = id, newMemo(), map[string]int{}
		repeats = 0
	}
	m := g.scanMemo
	g.mu.Unlock()
	if scan.ScanID != "" && (now.Sub(start) > g.o.MaxScanAge || repeats >= g.o.MaxRepeats) {
		// An exhausted partial page discards staging; the next scan starts afresh.
		return extension.Snapshot{ScanID: id}, nil
	}
	n := &normalizer{s: g.s, c: g.c, m: m, received: now}
	snap, err := g.scanPage(ctx, n, c, horizon)
	if err != nil {
		return extension.Snapshot{}, scanFailure(err)
	}
	snap.ScanID, snap.Checkpoint = id, id
	if g.o.Observed != nil {
		g.o.Observed(ctx, snap.Observations)
	}
	g.mu.Lock()
	if g.memoID == id {
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
	case phaseOpenIssues, phaseClosedIssues:
		obs, count, more, next, err = g.issuesPage(ctx, n, c, horizon, b)
	case phaseOpenPulls, phaseClosedPulls:
		obs, count, more, next, err = g.pullsPage(ctx, n, c, horizon, b)
	case phaseComments, phaseReviewComments:
		obs, count, more, next, err = g.commentsPage(ctx, n, c, horizon, b)
	default:
		return extension.Snapshot{}, extension.ErrInvalid
	}
	if err == nil {
		obs, err = n.withCopies(obs)
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

// listing is the query a phase lists issues or PRs with. Open items are
// listed oldest first, so an item opened during the scan lands after the
// cursor and one closed during it is observed by the closed listing that
// follows. Closed items are listed most recently updated first, so the
// listing can stop at the horizon; GitHub filters issues by it too.
func listing(phase string, horizon time.Time) string {
	switch phase {
	case phaseOpenIssues, phaseOpenPulls:
		return "?state=open&sort=created&direction=asc"
	case phaseClosedIssues:
		return "?state=closed&sort=updated&direction=desc&since=" + url.QueryEscape(horizon.Format(time.RFC3339))
	}
	return "?state=closed&sort=updated&direction=desc"
}

// wanted reports whether a listed item is observed in this phase, and stop
// that the listing has passed the horizon, so no later item is (an item
// closes no later than its last update). A closed item the open listing
// already observed, because it closed during the generation, is left to
// the next one: observing it again, closed, would give one item two
// versions in one generation.
func wanted(m *memo, phase string, number int, state string, closedAt *time.Time, updated, horizon time.Time) (want, stop bool) {
	if phase == phaseOpenIssues || phase == phaseOpenPulls {
		return state == "open", false
	}
	if updated.Before(horizon) {
		return false, true
	}
	return state == "closed" && closedAt != nil && !closedAt.Before(horizon) && !m.listed(number, false), false
}

func (g *Ingress) issuesPage(ctx context.Context, n *normalizer, c cursor, horizon time.Time, b *budget) ([]extension.Observation, int, bool, int, error) {
	items, more, err := page[rawIssue](ctx, g.c, g.s.Repo, repoPath(g.s.Repo, "/issues")+listing(c.phase, horizon), c.page, perPage)
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
		want, stop := wanted(n.m, c.phase, it.Number, it.State, it.ClosedAt, it.UpdatedAt, horizon)
		if stop {
			return out, len(items), false, len(items), nil
		}
		if it.PullRequest != nil || !want {
			continue
		}
		if it.State == "open" {
			n.m.listed(it.Number, true)
		}
		if it.State == "closed" || strings.EqualFold(it.StateReason, "reopened") {
			b.units++
		}
		reads := n.permissionReads()
		o, err := n.issueObservation(ctx, it)
		if err != nil {
			return nil, 0, false, 0, err
		}
		b.units += n.permissionReads() - reads
		out = append(out, o)
	}
	return out, len(items), more, i, nil
}

func (g *Ingress) pullsPage(ctx context.Context, n *normalizer, c cursor, horizon time.Time, b *budget) ([]extension.Observation, int, bool, int, error) {
	items, more, err := page[rawPull](ctx, g.c, g.s.Repo, repoPath(g.s.Repo, "/pulls")+listing(c.phase, horizon), c.page, perPage)
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
		want, stop := wanted(n.m, c.phase, p.Number, p.State, p.ClosedAt, p.UpdatedAt, horizon)
		if stop {
			return out, len(items), false, len(items), nil
		}
		if !want {
			continue
		}
		if p.State == "open" {
			n.m.listed(p.Number, true)
		}
		b.units += 4
		n.remember(p)
		reads := n.permissionReads()
		o, err := n.pullObservation(ctx, p)
		if err != nil {
			return nil, 0, false, 0, err
		}
		b.units += n.permissionReads() - reads
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
		conflicts, err := g.conflicts(ctx, n, p)
		if err != nil {
			return nil, 0, false, 0, err
		}
		out = append(out, conflicts...)
	}
	return out, len(items), more, i, nil
}

// conflicts reads an open PR that belongs to an issue (any open PR when the
// entry tracks pull requests) on its own, since only
// that read carries mergeable, and observes it if it has conflicts. GitHub
// sends no webhook when a PR's mergeability changes (the base branch moving
// under it), so each generation asks.
func (g *Ingress) conflicts(ctx context.Context, n *normalizer, p rawPull) ([]extension.Observation, error) {
	if p.State != "open" {
		return nil, nil
	}
	if _, ref, err := n.pullKey(ctx, p); err != nil || ref == nil && !g.s.TrackPulls {
		return nil, err
	}
	full, err := g.c.Pull(ctx, g.s.Repo, p.Number)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusGone) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, ok, err := n.conflictsObservation(ctx, full)
	if err != nil || !ok {
		return nil, err
	}
	return []extension.Observation{o}, nil
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

// scanFailure authors a failed page's cause, so the host records what GitHub
// said (a rate limit, an HTTP status and GitHub's message) rather than an
// unauthored failure. Both carry only GitHub's own message field, never the
// request or credentials. The cause stays reachable with errors.As; other
// errors stay unauthored and reach the extension's log.
func scanFailure(err error) error {
	var limit *RateLimitError
	var status *StatusError
	switch {
	case errors.As(err, &limit):
		return authoredScanError{&extension.OperationError{Code: "rate_limited", Message: limit.Error()}, err}
	case errors.As(err, &status):
		return authoredScanError{&extension.OperationError{Code: "http_" + strconv.Itoa(status.Status), Message: status.Error()}, err}
	}
	return err
}

// authoredScanError is a scan failure's authored form and its cause.
type authoredScanError struct {
	op    *extension.OperationError
	cause error
}

func (e authoredScanError) Error() string   { return e.op.Error() }
func (e authoredScanError) Unwrap() []error { return []error{e.op, e.cause} }
