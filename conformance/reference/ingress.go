package reference

import (
	"context"
	"fmt"
	"sort"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Ingress is a synthetic service-free fixture. Both paths use caller-supplied
// canonical observations; no service payload or runtime diff implementation.
type Ingress struct {
	Declaration extension.IngressSpec
	PushResult  extension.PushResult
	Pages       map[string]extension.Snapshot
}

func (i *Ingress) Spec() extension.IngressSpec { return i.Declaration }
func (i *Ingress) Push(ctx context.Context, _ extension.Source, _ extension.Delivery) (extension.PushResult, error) {
	return i.PushResult, ctx.Err()
}
func (i *Ingress) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return extension.Snapshot{}, err
	}
	p, ok := i.Pages[s.Cursor]
	if !ok {
		return extension.Snapshot{}, extension.ErrInvalid
	}
	return p, nil
}

// ObservationSet is a reference diff oracle. Production diffing is 02-02.
// A generation is staged across pages, then published only on exhaustive finish.
type ObservationSet struct {
	Current      map[string]extension.Observation
	staged       map[string]extension.Observation
	scan, cursor string
}

func (s *ObservationSet) Page(p extension.Snapshot) (changed []domain.Event, absent []string, err error) {
	if p.ScanID == "" || p.Complete && p.NextCursor != "" {
		return nil, nil, extension.ErrInvalid
	}
	if s.scan != "" && s.scan != p.ScanID {
		return nil, nil, fmt.Errorf("%w: changed generation", extension.ErrInvalid)
	}
	if p.NextCursor != "" && p.NextCursor == s.cursor {
		return nil, nil, extension.ErrInvalid
	}
	// Validate before mutating so malformed pages cannot poison a retry.
	pageVersions := map[string]string{}
	for _, o := range p.Observations {
		if o.ID == "" || o.Version == "" || o.Event.Source == "" || o.Event.DeliveryID == "" {
			return nil, nil, extension.ErrInvalid
		}
		if version, ok := pageVersions[o.ID]; ok && version != o.Version {
			return nil, nil, extension.ErrConflict
		}
		pageVersions[o.ID] = o.Version
		if old, ok := s.staged[o.ID]; ok && old.Version != o.Version {
			return nil, nil, extension.ErrConflict
		}
	}
	if s.staged == nil {
		s.staged = map[string]extension.Observation{}
	}
	s.scan, s.cursor = p.ScanID, p.NextCursor
	for _, o := range p.Observations {
		s.staged[o.ID] = o
	}
	if !p.Complete {
		return nil, nil, nil
	}
	for id, o := range s.staged {
		if old, ok := s.Current[id]; !ok || old.Version != o.Version {
			changed = append(changed, o.Event)
		}
	}
	for id := range s.Current {
		if _, ok := s.staged[id]; !ok {
			absent = append(absent, id)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].Source == changed[j].Source {
			return changed[i].DeliveryID < changed[j].DeliveryID
		}
		return changed[i].Source < changed[j].Source
	})
	sort.Strings(absent)
	s.Current, s.staged, s.scan, s.cursor = s.staged, nil, "", ""
	return changed, absent, nil
}
