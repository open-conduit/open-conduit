package storage_test

import (
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// selecting answers QueryWorks from a list, as storage.SelectWorks gives
// its meaning, and counts the Work it returns.
type selecting struct {
	storage.Tx
	works []domain.Work
	read  *int
}

func (t selecting) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	page, _ := storage.SelectWorks(t.works, q)
	*t.read += len(page)
	return page, nil
}

// LiveOrChanged reads the live Work and the finished Work changed since the
// cursor, once each in ID order; NextChangeCursor moves the cursor a lag
// behind the latest change read, never past finished Work left unsettled
// and never back.
func TestLiveOrChangedAndTheCursor(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	work := func(id string, state domain.WorkState, minute int) domain.Work {
		return domain.Work{ID: domain.ID(id), ScopeID: "eng", State: state, UpdatedAt: at.Add(time.Duration(minute) * time.Minute)}
	}
	works := []domain.Work{
		work("a-old", domain.WorkCompleted, 0),
		work("b-live", domain.WorkRunning, 1),
		work("c-new", domain.WorkFailed, 10),
		work("d-newer", domain.WorkCompleted, 20),
		{ID: "e-other", ScopeID: "ops", State: domain.WorkRunning},
	}
	read := 0
	tx := selecting{works: works, read: &read}
	got, err := storage.LiveOrChanged(tx, storage.WorkQuery{ScopeID: "eng"}, at.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ws []domain.Work) []domain.ID {
		var out []domain.ID
		for _, w := range ws {
			out = append(out, w.ID)
		}
		return out
	}
	if !slices.Equal(ids(got), []domain.ID{"b-live", "c-new", "d-newer"}) || read != 3 {
		t.Fatalf("read %v (%d records), want the live and the changed", ids(got), read)
	}
	if all, _ := storage.LiveOrChanged(tx, storage.WorkQuery{ScopeID: "eng"}, time.Time{}); len(all) != 4 {
		t.Fatalf("a first pass reads every Work of the query: %v", ids(all))
	}

	since := at.Add(5 * time.Minute)
	none := func(domain.Work) bool { return false }
	if next := storage.NextChangeCursor(since, got, none); !next.Equal(at.Add(20*time.Minute - storage.ChangeLag)) {
		t.Fatalf("settled: %v, want a lag behind the latest change", next)
	}
	if next := storage.NextChangeCursor(since, got, func(w domain.Work) bool { return w.ID == "c-new" }); !next.Equal(at.Add(10 * time.Minute)) {
		t.Fatalf("c-new unsettled: %v, want its change", next)
	}
	// Unsettled live Work holds nothing back: it is read every pass.
	if next := storage.NextChangeCursor(since, got, func(w domain.Work) bool { return w.ID == "b-live" }); !next.Equal(at.Add(20*time.Minute - storage.ChangeLag)) {
		t.Fatalf("live unsettled: %v", next)
	}
	// Nothing read later than the cursor allows: it stays.
	if next := storage.NextChangeCursor(at.Add(30*time.Minute), got, none); !next.Equal(at.Add(30 * time.Minute)) {
		t.Fatalf("moved back: %v", next)
	}
	if next := storage.NextChangeCursor(since, nil, none); !next.Equal(since) {
		t.Fatalf("nothing read: %v", next)
	}
}
