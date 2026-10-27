package bootstrap

import (
	"context"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	hu "github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
)

// Human decisions (issue #52). The `human` entry type is registered with the
// config registry and human.ask joins every generation's toolbelt (it
// refuses in a scope without an entry). A scope's entry is an action
// provider under its entry name, offering "answer" on Work parked on one of
// its asks, and annotates that Work with the question while it is open. A
// pass settles ask records (answered, expired, withdrawn). Nothing here
// projects to a Work's source.

const humanSyncGap = 5 * time.Second

type humanState struct {
	mu   sync.Mutex
	last time.Time
}

func humanRegistry(reg *config.Registry) {
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	reg.Extensions[hu.Type] = config.ExtensionType{Validate: hu.ValidateExtension}
	reg.Operations = append(reg.Operations, hu.Operations()...)
}

// humanEntry is the scope's `human` entry in the current generation, when
// the scope is enabled and has one.
func (d *daemon) humanEntry(scope domain.ID) (hu.Entry, bool) {
	s, _ := d.configs.Current()
	if s == nil || s.Scopes[scope] == nil || !s.Scopes[scope].Enabled {
		return hu.Entry{}, false
	}
	for _, x := range s.Scopes[scope].Extensions {
		if x.Type != hu.Type {
			continue
		}
		c, err := hu.ParseConfig(x.Config)
		if err != nil {
			return hu.Entry{}, false
		}
		return hu.Entry{Name: x.Name, Config: c}, true
	}
	return hu.Entry{}, false
}

// humanAnnotations show the Work's open question.
func (d *daemon) humanAnnotations(ctx context.Context, w domain.Work) []management.Annotation {
	entry, ok := d.humanEntry(w.ScopeID)
	if !ok {
		return nil
	}
	lines, ok := hu.Annotate(ctx, d.coord, entry, w, d.now())
	if !ok {
		return nil
	}
	out := make([]management.Annotation, 0, len(lines))
	for _, l := range lines {
		out = append(out, management.Annotation{Source: entry.Name, Name: l.Name, Value: l.Value})
	}
	return out
}

// syncHuman settles ask records, at most every humanSyncGap.
func (d *daemon) syncHuman(ctx context.Context) {
	d.human.mu.Lock()
	if d.now().Sub(d.human.last) < humanSyncGap {
		d.human.mu.Unlock()
		return
	}
	d.human.last = d.now()
	d.human.mu.Unlock()
	res, err := hu.Sync(ctx, d.coord, d.now())
	code := ""
	if err != nil {
		code = errorCode(err)
	}
	d.report("human_asks", "", code)
	if res.Late > 0 {
		d.log.Info("late answers recorded", "late", res.Late)
	}
}
