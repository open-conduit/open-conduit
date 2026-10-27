package integration_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDaemonArtifactCleanup kills the daemon, leaves what a killed writer and
// an interrupted publication leave in the artifact store, and restarts it:
// the unfinished stream is gone once the daemon is ready, aged unreferenced
// content goes on the first retention pass, and referenced or recent content
// stays.
func TestDaemonArtifactCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	d := newDrill(t)
	d.start("")
	if _, err := d.submit("drill.done"); err != nil {
		t.Fatal(err)
	}
	w := d.waitState("completed")
	xs := d.executions(w.ID)
	if len(xs) != 1 || len(xs[0].Artifacts) == 0 {
		t.Fatalf("expected one execution with artifacts: %+v", xs)
	}
	referenced := xs[0].Artifacts[0].ID
	d.kill()

	store := filepath.Join(d.state, "artifacts")
	old := time.Now().Add(-48 * time.Hour).UTC()
	// The referenced object is backdated so only the live set protects it.
	metaPath := filepath.Join(store, "objects", referenced, "meta.json")
	var meta map[string]any
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	meta["PublishedAt"] = old
	writePrivate(t, metaPath, meta)

	pending := filepath.Join(store, "pending", artifactID(t))
	if err = os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pending, "data"), []byte("half a wri"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged := plantObject(t, store, old)
	recent := plantObject(t, store, time.Now().UTC())

	d.start("")
	if _, err = os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("unfinished write survived restart: %v", err)
	}
	d.until("aged orphan collected", 20*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(store, "objects", aged))
		return os.IsNotExist(err)
	})
	for name, id := range map[string]string{"referenced": referenced, "recent": recent} {
		if _, err = os.Stat(filepath.Join(store, "objects", id, "data")); err != nil {
			t.Fatalf("%s object was removed: %v", name, err)
		}
	}
}

func artifactID(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// plantObject publishes an object no execution references, as an
// interrupted Commit or an unattached publication leaves one.
func plantObject(t *testing.T, store string, at time.Time) string {
	t.Helper()
	id := artifactID(t)
	dir := filepath.Join(store, "objects", id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("orphan")
	if err := os.WriteFile(filepath.Join(dir, "data"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	writePrivate(t, filepath.Join(dir, "meta.json"), map[string]any{
		"Ref":         map[string]any{"ID": id, "Size": len(data), "MediaType": "text/plain"},
		"SHA256":      hex.EncodeToString(sum[:]),
		"PublishedAt": at,
	})
	return id
}

func writePrivate(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
