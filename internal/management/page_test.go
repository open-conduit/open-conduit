package management

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// lazyPage pages over items built on demand: the items build drops are
// neither returned nor counted against the limit, a cursor carries on after
// the last item returned, and a page builds only the items it reaches (its
// own, those dropped among them, and the one that shows there is more).
func TestLazyPageBuildsOnlyThePage(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Server{o: Options{Now: func() time.Time { return now }, CursorKey: []byte("key")}}
	var built []int
	items := func() []lazyKeyed {
		var out []lazyKeyed
		for i := range 10 {
			out = append(out, lazyKeyed{key: fmt.Sprintf("%020d/%d", i, i), build: func() (any, bool, error) {
				built = append(built, i)
				return i, i%3 != 1, nil // 1, 4 and 7 drop out
			}})
		}
		// The page sorts by key, whatever order the items come in.
		slices.Reverse(out)
		return out
	}
	bound := s.nowBound()
	var got []any
	q := listQuery{limit: 3}
	for pages := 1; ; pages++ {
		built = nil
		out, err := s.lazyPage("runs", q, bound, items())
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out["items"].([]any)...)
		if pages == 1 && !slices.Equal(built, []int{0, 1, 2, 3, 4, 5}) {
			t.Fatalf("first page built %v, want up to the item after it", built)
		}
		next := out["next_cursor"].(string)
		if next == "" {
			if pages != 3 {
				t.Fatalf("%d pages, want 3", pages)
			}
			break
		}
		c, err := s.open(next)
		if err != nil {
			t.Fatal(err)
		}
		q.cursor = &c
	}
	if want := []any{0, 2, 3, 5, 6, 8, 9}; !slices.Equal(got, want) {
		t.Fatalf("pages returned %v, want %v", got, want)
	}
}
