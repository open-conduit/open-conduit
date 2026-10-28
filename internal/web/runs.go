package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// runsPage is the Runs page (10-04). Placeholder until the port lands.
func (s *Server) runsPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	wd := s.cachedWorld(ctx, true)
	s.render(w, r, "runs", &page{Title: "Runs", Nav: "runs"}, wd)
}

// bulkControl holds or releases the selected Work (10-04). Placeholder.
func (s *Server) bulkControl(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

// ago renders a timestamp relative to now: "just now", "4m ago", "3h ago",
// then the date.
func ago(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	case d < 7*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
	return t.Local().Format("2 Jan")
}

// summary is a run's outcome without its kind when the state already
// says it ("failed: the build broke" shows as "the build broke").
func summary(r client.Run) string {
	if o := r.Outcome; o != nil && o.Kind == r.State && o.Summary != "" {
		return o.Summary
	}
	return runview.Outcome(r)
}

// plural counts a noun: "1 run", "3 runs".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
