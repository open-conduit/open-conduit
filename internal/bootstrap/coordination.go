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

	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/storage"

	// SQLite is the default backend and is always built in. Another
	// backend is offered by building its package into the binary, which
	// registers it with storage.RegisterBackend from its init.
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// storageFile is a state directory's choice of coordination backend and
// artifact backend. It lives in the state directory rather than the config
// file because the config's generations are stored in the coordination
// state itself, and because the offline state commands (backup, state
// check, auth recover) have only the state directory to go on. Without it
// the directory uses SQLite at conduit.db and the file store at artifacts.
const storageFile = "storage.json"

// storageChoice is storage.json:
//
//	{"backend": "sqlite", "location": "/abs/path/conduit.db",
//	 "artifacts": {"backend": "files", "location": "/abs/path/artifacts"}}
//
// backend names a registered storage.Backend; location is handed to it
// verbatim (an absolute path, a connection string), and the backend
// refuses one it cannot resolve the same way from every process. An
// omitted location is <state>/conduit.db. The backend's Open owns what
// the location names, so a location two state directories share is open
// in one daemon at a time. artifacts names a registered
// storage.ArtifactBackend and its location the same way; without it the
// store is the file store at <state>/artifacts.
type storageChoice struct {
	Backend   string           `json:"backend"`
	Location  string           `json:"location,omitempty"`
	Artifacts *artifactsChoice `json:"artifacts,omitempty"`
}

// artifactsChoice is storage.json's artifact backend.
type artifactsChoice struct {
	Backend  string `json:"backend"`
	Location string `json:"location,omitempty"`
}

// coordination is a state directory's resolved coordination backend.
type coordination struct {
	name     string
	backend  storage.Backend
	location string
}

// artifactStorage is a state directory's resolved artifact backend.
type artifactStorage struct {
	name     string
	backend  storage.ArtifactBackend
	location string
}

// readChoice reads stateDir's storage.json, or the zero choice without one.
func readChoice(stateDir string) (storageChoice, error) {
	var choice storageChoice
	data, err := os.ReadFile(filepath.Join(stateDir, storageFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return choice, nil
	case err != nil:
		return choice, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&choice); err != nil {
		return choice, fmt.Errorf("%s: %w", storageFile, err)
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return choice, fmt.Errorf("%s: trailing data", storageFile)
	}
	if choice.Backend == "" {
		return choice, fmt.Errorf("%s: backend is required", storageFile)
	}
	if choice.Artifacts != nil && choice.Artifacts.Backend == "" {
		return choice, fmt.Errorf("%s: artifacts: backend is required", storageFile)
	}
	return choice, nil
}

// coordinationOf reads stateDir's storage choice and finds its backend.
func coordinationOf(stateDir string) (coordination, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return coordination{}, err
	}
	c := coordination{name: sqlite.Name, location: filepath.Join(stateDir, "conduit.db")}
	choice, err := readChoice(stateDir)
	if err != nil {
		return c, err
	}
	if choice.Backend != "" {
		c.name = choice.Backend
	}
	if choice.Location != "" {
		c.location = choice.Location
	}
	if c.backend, err = storage.LookupBackend(c.name); err != nil {
		return c, fmt.Errorf("%s: %w", storageFile, err)
	}
	return c, nil
}

// artifactsOf reads stateDir's storage choice and finds its artifact
// backend. An unknown one is an error: nothing falls back to the file
// store, which would start an empty store beside the real one.
func artifactsOf(stateDir string) (artifactStorage, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return artifactStorage{}, err
	}
	a := artifactStorage{name: artifacts.BackendName, location: filepath.Join(stateDir, "artifacts")}
	choice, err := readChoice(stateDir)
	if err != nil {
		return a, err
	}
	if choice.Artifacts != nil {
		a.name = choice.Artifacts.Backend
		if choice.Artifacts.Location != "" {
			a.location = choice.Artifacts.Location
		}
	}
	if a.backend, err = storage.LookupArtifactBackend(a.name); err != nil {
		return a, fmt.Errorf("%s: artifacts: %w", storageFile, err)
	}
	return a, nil
}

func (c coordination) open(ctx context.Context) (storage.Database, error) {
	return c.backend.Open(ctx, c.location)
}

func (a artifactStorage) open(ctx context.Context) (storage.ArtifactStore, error) {
	return a.backend.Open(ctx, a.location)
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

// openArtifacts opens stateDir's artifact store with its chosen backend.
func openArtifacts(ctx context.Context, stateDir string) (artifactStorage, storage.ArtifactStore, error) {
	a, err := artifactsOf(stateDir)
	if err != nil {
		return a, nil, err
	}
	s, err := a.open(ctx)
	return a, s, err
}

// restoreTarget is where a restore puts a bundle's artifacts: the file
// store in the restored directory by default, or the backend and location
// the operator names (cli.RestoreOptions). The backend is looked up before
// anything is restored, so an unknown one refuses the restore.
func restoreTarget(o cli.RestoreOptions) (backup.ArtifactTarget, *artifactsChoice, error) {
	if o.ArtifactBackend == "" {
		if o.ArtifactLocation != "" {
			return backup.ArtifactTarget{}, nil, fmt.Errorf("an artifact location needs its backend (--artifacts NAME)")
		}
		return backup.ArtifactTarget{Backend: artifacts.Backend}, nil, nil
	}
	b, err := storage.LookupArtifactBackend(o.ArtifactBackend)
	if err != nil {
		return backup.ArtifactTarget{}, nil, err
	}
	if o.ArtifactLocation == "" && o.ArtifactBackend != artifacts.BackendName {
		return backup.ArtifactTarget{}, nil, fmt.Errorf("artifact backend %q needs a location (--artifacts-location)", o.ArtifactBackend)
	}
	choice := &artifactsChoice{Backend: o.ArtifactBackend, Location: o.ArtifactLocation}
	if o.ArtifactBackend == artifacts.BackendName && o.ArtifactLocation == "" {
		// The default store: nothing to record.
		choice = nil
	}
	return backup.ArtifactTarget{Backend: b, Location: o.ArtifactLocation}, choice, nil
}

// recordChoice writes a restored directory's storage choice when its
// bundle's backend is not the default, so the directory opens with the
// backend that restored it. backup.Restore calls it on the staged
// directory, before publishing, so the location is left to its default
// (<state>/conduit.db, where the snapshot was restored).
func recordChoice(stateDir, backendName string) error {
	return recordChoiceWith(nil)(stateDir, backendName)
}

// recordChoiceWith is recordChoice that also records the artifact backend
// a restore imported into, when it is not the default.
func recordChoiceWith(art *artifactsChoice) backup.Record {
	return func(stateDir, backendName string) error {
		if backendName == "" {
			backendName = sqlite.Name
		}
		if backendName == sqlite.Name && art == nil {
			return nil
		}
		data, err := json.Marshal(storageChoice{Backend: backendName, Artifacts: art})
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(stateDir, storageFile), append(data, '\n'), 0o600)
	}
}

// restoreBackends are the backends a bundle can be restored with: those
// built into this binary.
var restoreBackends backup.Backends = storage.LookupBackend
