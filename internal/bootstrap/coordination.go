package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/storage"

	// SQLite is the default backend and is always built in. Another
	// backend is offered by building its package into the binary, which
	// registers it with storage.RegisterBackend from its init.
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// storageFile is a state directory's choice of coordination backend. It
// lives in the state directory rather than the config file because the
// config's generations are stored in the coordination state itself, and
// because the offline state commands (backup, state check, auth recover)
// have only the state directory to go on. Without it the directory uses
// SQLite at conduit.db.
const storageFile = "storage.json"

// storageChoice is storage.json:
//
//	{"backend": "sqlite", "location": "/abs/path/conduit.db"}
//
// backend names a registered storage.Backend; location is handed to it
// verbatim (an absolute path, a connection string), and the backend
// refuses one it cannot resolve the same way from every process. An
// omitted location is <state>/conduit.db. The backend's Open owns what
// the location names, so a location two state directories share is open
// in one daemon at a time.
type storageChoice struct {
	Backend  string `json:"backend"`
	Location string `json:"location,omitempty"`
}

// coordination is a state directory's resolved coordination backend.
type coordination struct {
	name     string
	backend  storage.Backend
	location string
}

// coordinationOf reads stateDir's storage choice and finds its backend.
func coordinationOf(stateDir string) (coordination, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return coordination{}, err
	}
	c := coordination{name: sqlite.Name, location: filepath.Join(stateDir, "conduit.db")}
	data, err := os.ReadFile(filepath.Join(stateDir, storageFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		var choice storageChoice
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&choice); err != nil {
			return c, fmt.Errorf("%s: %w", storageFile, err)
		}
		if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("%s: trailing data", storageFile)
		}
		if choice.Backend == "" {
			return c, fmt.Errorf("%s: backend is required", storageFile)
		}
		c.name = choice.Backend
		if choice.Location != "" {
			c.location = choice.Location
		}
	}
	if c.backend, err = storage.LookupBackend(c.name); err != nil {
		return c, fmt.Errorf("%s: %w", storageFile, err)
	}
	return c, nil
}

func (c coordination) open(ctx context.Context) (storage.Database, error) {
	return c.backend.Open(ctx, c.location)
}

// openState opens stateDir's coordination state with its chosen backend.
func openState(ctx context.Context, stateDir string) (coordination, storage.Database, error) {
	c, err := coordinationOf(stateDir)
	if err != nil {
		return c, nil, err
	}
	db, err := c.open(ctx)
	return c, db, err
}

// recordChoice writes a restored directory's storage choice when its
// bundle's backend is not the default, so the directory opens with the
// backend that restored it. backup.Restore calls it on the staged
// directory, before publishing, so the location is left to its default
// (<state>/conduit.db, where the snapshot was restored).
func recordChoice(stateDir, backendName string) error {
	if backendName == sqlite.Name || backendName == "" {
		return nil
	}
	data, err := json.Marshal(storageChoice{Backend: backendName})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, storageFile), append(data, '\n'), 0o600)
}

// restoreBackends are the backends a bundle can be restored with: those
// built into this binary.
var restoreBackends backup.Backends = storage.LookupBackend
