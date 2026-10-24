package reference

import (
	"context"
	"sync"

	"github.com/alsiola/open-conduit/observability"
)

type Observer struct {
	mu      sync.Mutex
	Secrets []string
	records []observability.Record
}

func (o *Observer) Observe(ctx context.Context, r observability.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := observability.Validate(r); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records = append(o.records, observability.Redact(r, o.Secrets))
	return nil
}
func (o *Observer) Records() []observability.Record {
	o.mu.Lock()
	defer o.mu.Unlock()
	records := make([]observability.Record, len(o.records))
	for i, r := range o.records {
		records[i] = observability.Redact(r, nil)
	}
	return records
}
