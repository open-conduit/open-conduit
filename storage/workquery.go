package storage

import (
	"slices"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// WorkOrder is the order QueryWorks lists Work in: by CreatedAt or
// UpdatedAt, each with the ID to break ties, ascending or descending.
type WorkOrder string

const (
	WorkCreated     WorkOrder = "created"
	WorkCreatedDesc WorkOrder = "-created"
	WorkUpdated     WorkOrder = "updated"
	WorkUpdatedDesc WorkOrder = "-updated"
)

// Valid says whether o is one of the orders; "" is WorkCreated.
func (o WorkOrder) Valid() bool {
	return o == "" || o == WorkCreated || o == WorkCreatedDesc || o == WorkUpdated || o == WorkUpdatedDesc
}

// Descending says whether o lists the latest first.
func (o WorkOrder) Descending() bool { return o == WorkCreatedDesc || o == WorkUpdatedDesc }

// Key is w's position in o.
func (o WorkOrder) Key(w domain.Work) WorkKey {
	if o == WorkUpdated || o == WorkUpdatedDesc {
		return WorkKey{At: w.UpdatedAt, ID: w.ID}
	}
	return WorkKey{At: w.CreatedAt, ID: w.ID}
}

// WorkKey is a Work's position in a WorkOrder: its time, to the
// nanosecond, then its ID.
type WorkKey struct {
	At time.Time
	ID domain.ID
}

// Before says whether k comes before other in ascending order.
func (k WorkKey) Before(other WorkKey) bool {
	a, b := k.At.UnixNano(), other.At.UnixNano()
	return a < b || a == b && k.ID < other.ID
}

// WorkQuery selects Work by what storage indexes. Each field left zero
// selects every Work.
type WorkQuery struct {
	// States are the states Work may be in, any of them.
	States []domain.WorkState
	// ScopeID, Kind and ParentID are the Work's own fields.
	ScopeID  domain.ID
	Kind     string
	ParentID domain.ID
	// Sources are WorkSource values Work may have, any of them.
	Sources []string
	// Executor is the Work's LatestExecutor, the Executor recorded on its
	// latest attempt;
	// Work with no attempt, or whose latest has none recorded, has none.
	Executor string
	// UpdatedSince keeps Work whose UpdatedAt is not before it.
	UpdatedSince time.Time
	// Order, After and Limit page QueryWorks: Work in Order strictly
	// after After, at most Limit of them (zero is all). CountWorks and
	// WorkFacets ignore them.
	Order WorkOrder
	After *WorkKey
	Limit int
}

// WorkFacets is what the Work a query selects can be told apart by: how
// many there are, and the distinct values of their scopes, sources
// (WorkSource) and latest attempts' executors, each sorted. A Work with no
// executor recorded adds none.
type WorkFacets struct {
	Count     int
	ScopeIDs  []domain.ID
	Sources   []string
	Executors []string
}

// WorkSource is where Work came from: the Source of its subject key
// (domain.SubjectKey: its first key other than its lineage key), else its
// Kind, so where Work came from doesn't change when it first waits or asks
// for child Work.
func WorkSource(w domain.Work) string {
	if k, ok := domain.SubjectKey(w.CorrelationKeys); ok {
		return k.Source
	}
	return w.Kind
}

// Matches says whether w is one q selects. It ignores Order, After and
// Limit.
func (q WorkQuery) Matches(w domain.Work) bool {
	return (len(q.States) == 0 || slices.Contains(q.States, w.State)) &&
		(q.ScopeID == "" || w.ScopeID == q.ScopeID) && (q.Kind == "" || w.Kind == q.Kind) &&
		(q.ParentID == "" || w.ParentID == q.ParentID) &&
		(len(q.Sources) == 0 || slices.Contains(q.Sources, WorkSource(w))) &&
		(q.Executor == "" || w.LatestExecutor == q.Executor) &&
		(q.UpdatedSince.IsZero() || !w.UpdatedAt.Before(q.UpdatedSince))
}

// SelectWorks answers QueryWorks, CountWorks and WorkFacets over every
// Work in works by reading them all. It is the contract's meaning, for a backend with no index to
// read through and for tests; its cost follows works, not the answer.
func SelectWorks(works []domain.Work, q WorkQuery) (page []domain.Work, facets WorkFacets) {
	scopes, sources, executors := map[domain.ID]bool{}, map[string]bool{}, map[string]bool{}
	var in []domain.Work
	for _, w := range works {
		ex := w.LatestExecutor
		if !q.Matches(w) {
			continue
		}
		in = append(in, w)
		scopes[w.ScopeID] = true
		sources[WorkSource(w)] = true
		if ex != "" {
			executors[ex] = true
		}
	}
	facets = WorkFacets{Count: len(in), ScopeIDs: sortedKeys(scopes), Sources: sortedKeys(sources), Executors: sortedKeys(executors)}
	sort.Slice(in, func(i, j int) bool {
		a, b := q.Order.Key(in[i]), q.Order.Key(in[j])
		if q.Order.Descending() {
			return b.Before(a)
		}
		return a.Before(b)
	})
	page = []domain.Work{}
	for _, w := range in {
		if k := q.Order.Key(w); q.After != nil && !(q.Order.Descending() && k.Before(*q.After) || !q.Order.Descending() && q.After.Before(k)) {
			continue
		}
		if q.Limit > 0 && len(page) == q.Limit {
			break
		}
		page = append(page, w)
	}
	return page, facets
}

func sortedKeys[K ~string](m map[K]bool) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
