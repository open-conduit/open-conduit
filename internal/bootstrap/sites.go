package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// siteLedger is the host's durable record of the sites it gave each
// resource type in an acquisition (extension.ResourceAcquisition.Site):
// <state>/sites/<type>.json, the site IDs sorted. The host resolves a
// site for a resource provider later (extension.SiteResolver) only when
// it is recorded here for the provider's type, so a provider reaches only
// machines the host placed its own acquisitions on.
type siteLedger struct {
	dir string
	mu  sync.Mutex
}

func (l *siteLedger) path(typ string) string {
	return filepath.Join(l.dir, typ+".json")
}

func (l *siteLedger) read(typ string) ([]string, error) {
	b, err := os.ReadFile(l.path(typ))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, fmt.Errorf("the record of sites given to %s is unreadable: %w", typ, err)
	}
	return ids, nil
}

// give records that an acquisition of typ was bound at site. It is
// written before the acquisition runs, so a provider that made something
// there can always reach it again.
func (l *siteLedger) give(typ, site string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids, err := l.read(typ)
	if err != nil {
		return err
	}
	if slices.Contains(ids, site) {
		return nil
	}
	ids = append(ids, site)
	slices.Sort(ids)
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	return atomicWrite(l.path(typ), b)
}

// given reports whether the host gave typ site in an acquisition. A
// record it can't read gives nothing.
func (l *siteLedger) given(typ, site string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids, err := l.read(typ)
	return err == nil && slices.Contains(ids, site)
}
