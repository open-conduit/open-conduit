package tracker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// DeliveryContentType marks a Delivery body produced by VerifyDelivery for
// tracker type typ.
func DeliveryContentType(typ string) string {
	return "application/vnd.open-conduit." + typ + "-delivery+json"
}

// IngressOptions tune scan pacing (zero values select defaults).
type IngressOptions struct {
	// BatchUnits bounds detail reads (comments, reopen history) per page.
	BatchUnits int
	// PageBudget bounds wall time spent on one page before yielding a cursor.
	PageBudget time.Duration
	// MaxScanAge restarts a generation that has not completed in time.
	MaxScanAge time.Duration
	// MaxRepeats restarts a generation whose same page keeps being
	// re-requested after a successful answer (the runtime rejected it).
	MaxRepeats int
}

type history struct {
	updated time.Time
	reopens []time.Time
}

// Ingress is one tracker entry's (push, reconcile) pair.
type Ingress struct {
	s   Settings
	svc Service
	now func() time.Time
	o   IngressOptions

	mu       sync.Mutex
	answered map[string]int
	memoID   string
	reopens  map[string]history
}

var _ extension.Ingress = (*Ingress)(nil)

// NewIngress binds an entry to its Service.
func NewIngress(s Settings, svc Service, now func() time.Time, o IngressOptions) *Ingress {
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
	return &Ingress{s: s, svc: svc, now: now, o: o, answered: map[string]int{}, reopens: map[string]history{}}
}

// Settings returns the bound entry settings.
func (g *Ingress) Settings() Settings { return g.s }

// Service returns the bound Service.
func (g *Ingress) Service() Service { return g.svc }

// Spec declares every family; only raw deliveries are non-re-derivable.
func (g *Ingress) Spec() extension.IngressSpec {
	return extension.IngressSpec{Families: Families(g.s.Type)}
}

func (g *Ingress) checkSource(src extension.Source) error {
	if src.Namespace != g.s.Namespace || string(src.ScopeID) != g.s.Scope {
		return extension.ErrInvalid
	}
	return nil
}

// VerifyDelivery authenticates a raw webhook request through the Service
// and wraps what it reports into the Delivery that Push accepts. This is the
// transport-authentication boundary; Push itself trusts its caller.
func VerifyDelivery(typ string, svc Service, secret string, h map[string][]string, body []byte, received time.Time) (extension.Delivery, error) {
	if secret == "" {
		return extension.Delivery{}, ErrSignature
	}
	hook, err := svc.ParseHook(h, body, secret, received)
	if err != nil {
		return extension.Delivery{}, err
	}
	if hook.ID == "" {
		hook.ID = digest(string(body))[:32]
	}
	if len(hook.ID) > 128 || strings.ContainsAny(hook.ID, " \t\r\n/") {
		return extension.Delivery{}, extension.ErrInvalid
	}
	wrapped, err := json.Marshal(hook)
	if err != nil {
		return extension.Delivery{}, extension.ErrInvalid
	}
	return extension.Delivery{ID: hook.ID, ReceivedAt: received.UTC(), ContentType: DeliveryContentType(typ), Body: wrapped}, nil
}

// prime fills a normalizer with cached reopen history still valid for the
// issue's update time (a reopening always changes it).
func (g *Ingress) prime(n *normalizer, i Issue) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h, ok := g.reopens[i.Key]; ok && !i.UpdatedAt.IsZero() && h.updated.Equal(i.UpdatedAt) {
		n.reopens[i.Key] = h.reopens
	}
}

func (g *Ingress) remember(n *normalizer, i Issue) {
	r, ok := n.reopens[i.Key]
	if !ok || i.UpdatedAt.IsZero() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.reopens) > 20000 {
		g.reopens = map[string]history{}
	}
	g.reopens[i.Key] = history{updated: i.UpdatedAt, reopens: r}
}

// Push normalizes one verified delivery: it reads the issues and comments
// the delivery reports back from the service (read-only), so push yields the
// same observations reconcile would, plus the one-shot raw delivery Event.
func (g *Ingress) Push(ctx context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out extension.PushResult
	if err := g.checkSource(src); err != nil {
		return out, err
	}
	if d.ContentType != DeliveryContentType(g.s.Type) {
		return out, extension.ErrInvalid
	}
	var hook Hook
	if err := json.Unmarshal(d.Body, &hook); err != nil || hook.ID == "" || hook.ID != d.ID {
		return out, extension.ErrInvalid
	}
	received := d.ReceivedAt.UTC()
	if received.IsZero() {
		received = g.now().UTC()
	}
	n := newNormalizer(g.s, g.svc, received)
	seen := map[string]bool{}
	var issues, comments []string
	for _, key := range hook.Issues {
		key = strings.ToUpper(key)
		if seen[key] || !ValidKey(key) || !g.svc.Owns(key) {
			continue
		}
		seen[key] = true
		issues = append(issues, key)
		i, err := g.svc.Issue(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return extension.PushResult{}, err
		}
		g.prime(n, i)
		o, err := n.issueObservation(ctx, i)
		if err != nil {
			return extension.PushResult{}, err
		}
		g.remember(n, i)
		out.Observations = append(out.Observations, o)
	}
	for _, ref := range hook.Comments {
		if ref.ID == "" || seen["comment/"+ref.ID] {
			continue
		}
		seen["comment/"+ref.ID] = true
		if ref.Issue != "" && (!ValidKey(ref.Issue) || !g.svc.Owns(strings.ToUpper(ref.Issue))) {
			continue
		}
		key, c, err := g.svc.ReadComment(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return extension.PushResult{}, err
		}
		key = strings.ToUpper(key)
		if !ValidKey(key) || !g.svc.Owns(key) {
			continue
		}
		comments = append(comments, c.ID)
		o, err := n.commentObservation(ctx, key, c)
		if err != nil {
			return extension.PushResult{}, err
		}
		out.Observations = append(out.Observations, o)
	}
	t := TypesOf(g.s.Type)
	data, _ := json.Marshal(DeliveryPayload{Event: hook.Event, Delivery: hook.ID, Issues: issues, Comments: comments})
	delivery := "delivery/" + hook.ID
	out.Events = []domain.Event{{ID: domain.ID(g.s.Type + "-" + digest(g.s.Namespace, delivery)[:40]), Type: t.Delivery, Source: g.s.Namespace,
		DeliveryID: delivery, OccurredAt: received, ReceivedAt: received, Data: domain.Payload{Schema: t.SchemaDelivery, Data: data}}}
	return out, nil
}

// ---- reconcile ------------------------------------------------------------

type cursor struct {
	page   string // the Service's page cursor
	offset int
}

func (c cursor) String() string {
	return "p" + base64.RawURLEncoding.EncodeToString([]byte(c.page)) + ":" + strconv.Itoa(c.offset)
}

func parseCursor(s string) (cursor, bool) {
	rest, ok := strings.CutPrefix(s, "p")
	if !ok {
		return cursor{}, false
	}
	enc, off, ok := strings.Cut(rest, ":")
	if !ok {
		return cursor{}, false
	}
	page, err := base64.RawURLEncoding.DecodeString(enc)
	offset, err2 := strconv.Atoi(off)
	if err != nil || err2 != nil || offset < 0 || offset > 100000 {
		return cursor{}, false
	}
	return cursor{page: string(page), offset: offset}, true
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

// Reconcile returns one bounded page of a generation. A generation lists the
// entry's issues in stable creation order (open ones and those closed or
// updated within the horizon) and reads each listed issue's comments. Older
// closed issues age out as generic absence, which carries no meaning.
// Complete certifies that the listing was read to its end in this
// generation; rate limits and failed reads return errors, so no absence is
// inferred.
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
	c := cursor{}
	if scan.Cursor != "" {
		if c, ok = parseCursor(scan.Cursor); !ok {
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
	answerKey := scanID + "|" + c.String()
	repeats := g.answered[answerKey]
	if g.memoID != scanID {
		g.memoID, g.answered = scanID, map[string]int{}
		repeats = 0
	}
	g.mu.Unlock()
	if scan.ScanID != "" && (now.Sub(start) > g.o.MaxScanAge || repeats >= g.o.MaxRepeats) {
		// An exhausted partial page discards staging; the next scan starts afresh.
		return extension.Snapshot{ScanID: scanID}, nil
	}
	page, err := g.svc.ListIssues(ctx, start.Add(-g.s.ClosedHorizon), c.page)
	if err != nil {
		return extension.Snapshot{}, err
	}
	n := newNormalizer(g.s, g.svc, now)
	snap := extension.Snapshot{ScanID: scanID, Checkpoint: scanID}
	deadline := time.Now().Add(g.o.PageBudget)
	units := 0
	i := c.offset
	for ; i < len(page.Issues); i++ {
		if units >= g.o.BatchUnits || (units > 0 && time.Now().After(deadline)) {
			break
		}
		issue := page.Issues[i]
		if !ValidKey(issue.Key) || !g.svc.Owns(issue.Key) {
			continue
		}
		g.prime(n, issue)
		if _, cached := n.reopens[issue.Key]; !cached {
			units++
		}
		o, err := n.issueObservation(ctx, issue)
		if err != nil {
			return extension.Snapshot{}, err
		}
		g.remember(n, issue)
		snap.Observations = append(snap.Observations, o)
		comments, err := g.svc.Comments(ctx, issue.Key)
		units++
		if err != nil && !errors.Is(err, ErrNotFound) {
			return extension.Snapshot{}, err
		}
		sort.SliceStable(comments, func(a, b int) bool { return comments[a].CreatedAt.Before(comments[b].CreatedAt) })
		for _, cm := range comments {
			o, err := n.commentObservation(ctx, issue.Key, cm)
			if err != nil {
				continue // a comment without identity or timestamps is never observed
			}
			snap.Observations = append(snap.Observations, o)
		}
	}
	switch {
	case i < len(page.Issues):
		snap.NextCursor = cursor{page: c.page, offset: i}.String()
	case page.Next != "":
		if page.Next == c.page {
			return extension.Snapshot{}, errors.New("tracker listing repeated its cursor")
		}
		snap.NextCursor = cursor{page: page.Next}.String()
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
