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
	"github.com/open-conduit/open-conduit/protocol/artifactstore"
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
// store is the file store at <state>/artifacts. With a command, artifacts
// names a provider executable instead (protocol/artifactstore), so a store
// can be installed without rebuilding the binary:
//
//	"artifacts": {"backend": "s3", "command": "/usr/local/bin/oc-s3",
//	              "location": "s3://bucket/prefix"}
type storageChoice struct {
	Backend   string           `json:"backend"`
	Location  string           `json:"location,omitempty"`
	Artifacts *artifactsChoice `json:"artifacts,omitempty"`
}

// artifactsChoice is storage.json's artifact backend.
type artifactsChoice struct {
	Backend  string `json:"backend"`
	Location string `json:"location,omitempty"`
	// Command is the absolute path of a provider executable serving this
	// backend. A state directory is the operator's own: a restore writes
	// this from the operator's flags and never takes it from a bundle.
	Command string `json:"command,omitempty"`
}

// backend resolves the choice: its provider, or a built-in backend.
func (c artifactsChoice) resolve() (storage.ArtifactBackend, error) {
	if c.Command == "" {
		return storage.LookupArtifactBackend(c.Backend)
	}
	if !filepath.IsAbs(c.Command) {
		return nil, fmt.Errorf("artifact provider command %q is not an absolute path: %w", c.Command, storage.ErrInvalid)
	}
	if c.Location == "" {
		return nil, fmt.Errorf("artifact provider %q needs a location: %w", c.Backend, storage.ErrInvalid)
	}
	if _, err := storage.LookupArtifactBackend(c.Backend); err == nil {
		return nil, fmt.Errorf("artifact provider %q has the name of a built-in backend: %w", c.Backend, storage.ErrInvalid)
	}
	return artifactstore.Command(c.Command), nil
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
	resolved := artifactsChoice{Backend: a.name}
	if choice.Artifacts != nil {
		resolved = *choice.Artifacts
		a.name = resolved.Backend
		if resolved.Location != "" {
			a.location = resolved.Location
		}
	}
	if a.backend, err = resolved.resolve(); err != nil {
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

// openState opens stateDir's coordination state with its chosen backend. At a
// shared location it holds the instance lease until Close, refusing state
// another instance holds and taking over this host's own crashed tenure.
func openState(ctx context.Context, stateDir string) (coordination, storage.Database, error) {
	return openHeld(ctx, stateDir, nil)
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

// restoreTarget is where a restore puts a bundle: its database at the
// location the operator names, or in the restored directory, and its
// artifacts in the file store there, or the backend and location the
// operator names (cli.RestoreOptions). The artifact backend is looked up
// before anything is restored, so an unknown one refuses the restore.
func restoreTarget(o cli.RestoreOptions) (backup.Target, *artifactsChoice, error) {
	if o.ArtifactBackend == "" {
		if o.ArtifactLocation != "" || o.ArtifactCommand != "" {
			return backup.Target{}, nil, fmt.Errorf("an artifact location or command needs its backend (--artifacts NAME)")
		}
		return backup.Target{Database: o.DatabaseLocation, Artifacts: artifacts.Backend, EventArchive: sqlite.ArchiveBackend}, nil, nil
	}
	choice := &artifactsChoice{Backend: o.ArtifactBackend, Location: o.ArtifactLocation, Command: o.ArtifactCommand}
	if o.ArtifactLocation == "" && o.ArtifactBackend != artifacts.BackendName {
		return backup.Target{}, nil, fmt.Errorf("artifact backend %q needs a location (--artifacts-location)", o.ArtifactBackend)
	}
	b, err := choice.resolve()
	if err != nil {
		return backup.Target{}, nil, err
	}
	if o.ArtifactBackend == artifacts.BackendName && o.ArtifactLocation == "" {
		// The default store: nothing to record.
		choice = nil
	}
	return backup.Target{Database: o.DatabaseLocation, Artifacts: b, ArtifactLocation: o.ArtifactLocation, EventArchive: sqlite.ArchiveBackend}, choice, nil
}

// recordChoice writes a restored directory's storage choice when its
// bundle's backend is not the default, so the directory opens with the
// backend that restored it. backup.Restore calls it on the staged
// directory, before publishing, so the location is left to its default
// (<state>/conduit.db, where the snapshot was restored).
func recordChoice(stateDir, backendName string) error {
	return recordChoiceWith("", nil)(stateDir, backendName)
}

// recordChoiceWith is recordChoice that also records the database location
// a restore wrote to and the artifact backend it imported into, when they
// are not the defaults.
func recordChoiceWith(location string, art *artifactsChoice) backup.Record {
	return func(stateDir, backendName string) error {
		if backendName == "" {
			backendName = sqlite.Name
		}
		if backendName == sqlite.Name && location == "" && art == nil {
			return nil
		}
		data, err := json.Marshal(storageChoice{Backend: backendName, Location: location, Artifacts: art})
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(stateDir, storageFile), append(data, '\n'), 0o600)
	}
}

// restoreBackends are the backends a bundle can be restored with: those
// built into this binary.
var restoreBackends backup.Backends = storage.LookupBackend
