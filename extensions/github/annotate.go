package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// Annotation is one line of operator context about Work this entry owns.
type Annotation struct{ Name, Value string }

// Web is the browser URL of the client's host (https://github.com, or an
// Enterprise host).
func (c *Client) Web() string { return c.web }

// Annotate describes Work owned by this entry for operator views: the issue
// and its URL, whether it is a held issue, projection dry run and the
// corrections and problems its agent logged. It reads only durable state and
// makes no service calls. ok is false for Work this entry does not own.
func Annotate(ctx context.Context, store storage.Store, s Settings, web string, w domain.Work) ([]Annotation, bool) {
	n := 0
	for _, k := range w.CorrelationKeys {
		if k.Source != s.Namespace {
			continue
		}
		if num, _, ok := ParseIssueKey(k.ExternalID); ok {
			n = num
			break
		}
	}
	if n == 0 {
		return nil, false
	}
	out := []Annotation{
		{"issue", fmt.Sprintf("%s#%d", s.TrackerRepo, n)},
		{"url", fmt.Sprintf("%s/%s/issues/%d", strings.TrimRight(web, "/"), s.TrackerRepo, n)},
	}
	if w.Kind == WorkKindHeld && w.State == domain.WorkWaiting {
		out = append(out, Annotation{"held", "waiting for its time or conditions (scheduled label kept)"})
	}
	if s.Projection.DryRun {
		out = append(out, Annotation{"projection", "dry run: labels and comments are not written"})
	} else if s.Projection.Disabled {
		out = append(out, Annotation{"projection", "disabled"})
	}
	counts := map[string]int{}
	_ = store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		rows, err := tx.List(NotesNamespace, "")
		if err != nil {
			return err
		}
		for _, r := range rows {
			var note Note
			if json.Unmarshal(r.Value, &note) == nil && note.Work == string(w.ID) {
				counts[note.Kind]++
			}
		}
		return nil
	})
	for _, kind := range []string{"correction", "problem"} {
		if counts[kind] > 0 {
			out = append(out, Annotation{kind + "s", fmt.Sprint(counts[kind])})
		}
	}
	return out, true
}
