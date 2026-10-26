package integration_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// Every transaction uses a fresh connection/process-independent handle. The
// reference coordinator retains only options and fake time between operations;
// queue, causal budget, waits and checkpoints must survive in the real backend.
type reopening struct {
	mu   sync.Mutex
	path string
}

func (s *reopening) Transact(ctx context.Context, guard storage.Guard, fn func(storage.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := sqlite.Open(ctx, s.path)
	if err != nil {
		return err
	}
	err = db.Transact(ctx, guard, fn)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func TestRuntimeSpecificationOnReopenedSQLite(t *testing.T) {
	conformance.RunRuntime(t, func(options conformance.RuntimeOptions) conformance.RuntimeFixture {
		backend := &reopening{path: filepath.Join(t.TempDir(), "conduit.db")}
		return reference.NewRuntimeWithStore(options, backend, conformance.NewClock())
	})
}
