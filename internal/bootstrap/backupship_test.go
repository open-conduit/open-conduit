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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// memoryDestination is a destination in memory, whose answers a test can
// make wrong, as whoever controls a real one could.
type memoryDestination struct {
	mu      sync.Mutex
	files   map[string][]byte // bundle/path
	order   []string
	reads   []string
	failPut func(path string) error
	extra   []extension.BackupFile
	lists   int
}

func newMemoryDestination() *memoryDestination {
	return &memoryDestination{files: map[string][]byte{}}
}

func (m *memoryDestination) Put(_ context.Context, _ extension.BackupTarget, bundle string, f extension.BackupFile, r io.Reader) error {
	if m.failPut != nil {
		if err := m.failPut(f.Path); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[bundle+"/"+f.Path] = data
	m.order = append(m.order, f.Path)
	return nil
}

func (m *memoryDestination) List(context.Context, extension.BackupTarget) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lists++
	var out []string
	for k := range m.files {
		if b, ok := strings.CutSuffix(k, "/"+extension.BackupManifest); ok {
			out = append(out, b)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (m *memoryDestination) Files(_ context.Context, _ extension.BackupTarget, bundle string) ([]extension.BackupFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.extra)
	for k, v := range m.files {
		if p, ok := strings.CutPrefix(k, bundle+"/"); ok {
			out = append(out, extension.BackupFile{Path: p, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (m *memoryDestination) Read(_ context.Context, _ extension.BackupTarget, bundle, path string, offset int64, length int) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if offset == 0 {
		m.reads = append(m.reads, path)
	}
	data, ok := m.files[bundle+"/"+path]
	if !ok {
		return nil, false, errors.New("no such file")
	}
	end := min(offset+int64(length), int64(len(data)))
	return bytes.Clone(data[offset:end]), end == int64(len(data)), nil
}

// shipOwed runs the shipment the scheduler would start now, if one is due.
func shipOwed(ctx context.Context, d *daemon, now time.Time) {
	if name, ok := d.claimShipment(now); ok {
		d.shipBundle(ctx, name)
	}
}

func shipTo(d *daemon, m *memoryDestination) {
	d.reportedAt = map[string]time.Time{}
	d.backups.targets = func() []shipTarget {
		return []shipTarget{{cfg: config.BackupDestination{Name: "offsite", Type: "memory"}, d: m}}
	}
}

// A published bundle reaches the destination with its manifest last, so a
// bundle cut short is never one a restore can pick.
func TestScheduledBundleIsShippedManifestLast(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	m := newMemoryDestination()
	shipTo(d, m)
	if err := d.backupNow(ctx, start); err != nil {
		t.Fatal(err)
	}
	shipOwed(ctx, d, start)
	name := "conduit-20261102T090000Z"
	if got, _ := m.List(ctx, extension.BackupTarget{}); !slices.Equal(got, []string{name}) {
		t.Fatalf("listed %v", got)
	}
	if m.order[len(m.order)-1] != extension.BackupManifest {
		t.Fatalf("put order %v", m.order)
	}
	if !d.backups.shipDue.IsZero() || d.reportedCode("backup_destination", "offsite") != "" {
		t.Fatalf("a good shipment is owed again or reported: %v %q", d.backups.shipDue, d.reportedCode("backup_destination", "offsite"))
	}
}

// A failed upload doesn't fail the backup, is reported with how far it got,
// isn't listed, and is tried again until it lands.
func TestFailedShipmentIsReportedAndRetried(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	m := newMemoryDestination()
	m.failPut = func(p string) error {
		if p == extension.BackupManifest {
			return errors.New("bucket refused")
		}
		return nil
	}
	shipTo(d, m)
	if err := d.backupNow(ctx, start); err != nil {
		t.Fatalf("a failed shipment failed the backup: %v", err)
	}
	shipOwed(ctx, d, start)
	if got := d.reportedCode("backup_destination", "offsite"); got != "ship_failed" {
		t.Fatalf("diagnostic = %q", got)
	}
	if got, _ := m.List(ctx, extension.BackupTarget{}); len(got) != 0 {
		t.Fatalf("an incomplete bundle is listed: %v", got)
	}
	if r := d.backups.shipped["offsite"]; !strings.Contains(r.problem, "of") || !strings.Contains(r.problem, "files stored") {
		t.Fatalf("the outcome doesn't say how far it got: %+v", r)
	}
	if want := start.Add(backupRetry); !d.backups.shipDue.Equal(want) {
		t.Fatalf("retry at %v, want %v", d.backups.shipDue, want)
	}
	m.failPut = nil
	shipOwed(ctx, d, start.Add(backupRetry))
	if got, _ := m.List(ctx, extension.BackupTarget{}); len(got) != 1 {
		t.Fatalf("after the retry: %v", got)
	}
	if got := d.reportedCode("backup_destination", "offsite"); got != "" || !d.backups.shipDue.IsZero() {
		t.Fatalf("still reported after landing: %q, due %v", got, d.backups.shipDue)
	}
	// A bundle the destination holds isn't uploaded again.
	puts, lists := len(m.order), m.lists
	d.backups.shipDue = start
	shipOwed(ctx, d, start.Add(2*backupRetry))
	if len(m.order) != puts {
		t.Fatalf("a held bundle was uploaded again: %d puts, was %d", len(m.order), puts)
	}
	if m.lists != lists {
		t.Fatal("checking one bundle listed the whole destination")
	}
	// A held file at the wrong size is not held, so it is put again.
	m.mu.Lock()
	m.files["conduit-20261102T090000Z/"+extension.BackupManifest] = []byte("x")
	m.mu.Unlock()
	d.backups.shipDue = start
	shipOwed(ctx, d, start.Add(3*backupRetry))
	if len(m.order) == puts {
		t.Fatal("a bundle with a wrong-sized manifest counted as held")
	}
}

// hungDestination never answers until its context ends.
type hungDestination struct {
	*memoryDestination
	entered chan struct{}
}

func (h hungDestination) Files(ctx context.Context, _ extension.BackupTarget, _ string) ([]extension.BackupFile, error) {
	h.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A destination that never answers holds up neither publishing nor pruning,
// and the bundle it is shipping survives the pruning until it lets go.
func TestHungDestinationDoesNotStopBackups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	d.o.BackupsKept = 1
	h := hungDestination{newMemoryDestination(), make(chan struct{}, 1)}
	d.reportedAt = map[string]time.Time{}
	d.backups.targets = func() []shipTarget {
		return []shipTarget{{cfg: config.BackupDestination{Name: "offsite", Type: "memory"}, d: h}}
	}
	if err := d.backupNow(ctx, start); err != nil {
		t.Fatal(err)
	}
	first, ok := d.claimShipment(start)
	if !ok {
		t.Fatal("no shipment owed after a backup")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.shipBundle(ctx, first)
	}()
	<-h.entered
	for i := 1; i <= 2; i++ {
		if err := d.backupNow(ctx, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{first, "conduit-20261102T110000Z"}
	if got := bundleNames(t, d.o.BackupDir); !slices.Equal(got, want) {
		t.Fatalf("bundles %v, want %v", got, want)
	}
	cancel()
	<-done
	if d.backups.shipping != "" {
		t.Fatalf("claim kept after the shipment ended: %q", d.backups.shipping)
	}
}

// A call that goes quiet is given up on, and one that keeps making progress
// isn't, however long it takes in total.
func TestWithinStall(t *testing.T) {
	err := withinStall(context.Background(), time.Millisecond, func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, errShipStalled) {
		t.Fatalf("stalled call: %v", err)
	}
	err = withinStall(context.Background(), time.Hour, func(_ context.Context, progress func()) error {
		progress()
		return nil
	})
	if err != nil {
		t.Fatalf("call making progress: %v", err)
	}
}

// shippedBundle is a signed bundle stored at a destination.
func shippedBundle(t *testing.T) (*memoryDestination, string, string) {
	t.Helper()
	ctx := context.Background()
	f := newInPlaceFixture(t)
	private, public, err := backup.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OC_TEST_BACKUP_KEY", private)
	source := filepath.Join(t.TempDir(), "signed")
	if _, err := BackupState(ctx, f.state, source, "test", cli.BackupOptions{ConfigPath: trustConfig(t, "env:OC_TEST_BACKUP_KEY", public)}); err != nil {
		t.Fatal(err)
	}
	m := newMemoryDestination()
	if _, _, err := putBundle(ctx, shipTarget{cfg: config.BackupDestination{Name: "offsite"}, d: m}, source, "conduit-1", shipStallLimit); err != nil {
		t.Fatal(err)
	}
	return m, public, "conduit-1"
}

func TestFetchAuthenticatesTheBundle(t *testing.T) {
	ctx := context.Background()
	m, public, name := shippedBundle(t)
	_, otherPublic, err := backup.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := func() string { return filepath.Join(t.TempDir(), "fetched") }
	absent := filepath.Join(t.TempDir(), "absent.json")

	// Without a trust key a fetch is refused outright, though a local
	// restore with none is allowed.
	refused := dir()
	if _, err := fetchBundle(ctx, m, extension.BackupTarget{}, name, refused, cli.RestoreOptions{ConfigPath: absent}); !errors.Is(err, backup.ErrNotAuthentic) {
		t.Fatalf("no trust key: %v", err)
	}
	if len(m.reads) != 0 {
		t.Fatalf("fetched %v with no way to check them", m.reads)
	}

	// A key that didn't sign it stops the fetch after the manifest and
	// signature, before the database is read.
	if _, err := fetchBundle(ctx, m, extension.BackupTarget{}, name, refused, cli.RestoreOptions{ConfigPath: absent, TrustKeys: []string{otherPublic}}); !errors.Is(err, backup.ErrNotAuthentic) {
		t.Fatalf("wrong key: %v", err)
	}
	if slices.Contains(m.reads, "conduit.db") {
		t.Fatalf("read the database of an unauthenticated bundle: %v", m.reads)
	}
	if _, err := os.Lstat(refused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused fetch published: %v", err)
	}

	// The negative control: the genuine bundle fetches and restores.
	good := dir()
	info, err := fetchBundle(ctx, m, extension.BackupTarget{}, name, good, cli.RestoreOptions{ConfigPath: absent, TrustKeys: []string{public}})
	if err != nil || info.Authenticity != backup.Verified || info.Path != good {
		t.Fatalf("fetch = %+v, %v", info, err)
	}
	if _, err := RestoreState(ctx, good, filepath.Join(t.TempDir(), "restored"), cli.RestoreOptions{ConfigPath: absent, TrustKeys: []string{public}}); err != nil {
		t.Fatal(err)
	}

	// A swapped database still under the signed manifest is caught, and
	// nothing is published.
	key := name + "/conduit.db"
	genuine := m.files[key]
	m.files[key] = append(bytes.Clone(genuine[:len(genuine)-1]), genuine[len(genuine)-1]^0xff)
	swapped := dir()
	if _, err := fetchBundle(ctx, m, extension.BackupTarget{}, name, swapped, cli.RestoreOptions{ConfigPath: absent, TrustKeys: []string{public}}); err == nil {
		t.Fatal("a swapped database was accepted")
	}
	if _, err := os.Lstat(swapped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a bad bundle published: %v", err)
	}
}

// What a destination says about a bundle's files is untrusted.
// A bundle that holds the event archive survives the trip to a destination
// and back, so a host that lost its disk can still restore retired payloads.
func TestFetchBringsBackTheEventArchive(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	c, store, err := openState(ctx, f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var record domain.AcceptedEvent
	if err := store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "archive-scope", Name: "archive-scope"}, 0); err != nil {
			return err
		}
		now := time.Now().UTC()
		e := domain.Event{ID: "archived", Type: "fixture.seen", Source: "fixture", DeliveryID: "archived", OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "fixture.v1", Data: []byte(`{"kept":"in the archive"}`)}}
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		if err := tx.MarkRouted(e.ID, "", "unmatched"); err != nil {
			return err
		}
		var err error
		record, err = tx.Event(e.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	archive, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if err := archive.PutEvents(ctx, []domain.AcceptedEvent{record}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error { return tx.RetireEvent(record.Event.ID) }); err != nil {
		t.Fatal(err)
	}
	files, err := artifacts.Open(filepath.Join(f.state, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	source := filepath.Join(t.TempDir(), "bundle")
	if err := backup.CreateWithArchive(ctx, source, nil, c.name, store, files, archive, "test", retention(time.Now)); err != nil {
		t.Fatal(err)
	}
	m := newMemoryDestination()
	if _, _, err := putBundle(ctx, shipTarget{cfg: config.BackupDestination{Name: "offsite"}, d: m}, source, "conduit-1", shipStallLimit); err != nil {
		t.Fatal(err)
	}
	fetched := filepath.Join(t.TempDir(), "fetched")
	if _, err := fetchBundle(ctx, m, extension.BackupTarget{}, "conduit-1", fetched, cli.RestoreOptions{ConfigPath: filepath.Join(t.TempDir(), "absent.json"), AcceptUnsigned: true}); err != nil {
		t.Fatal(err)
	}
	segments, err := backup.SegmentNames(fetched)
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments = %v, %v", segments, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fetched), segments[0], backup.ArchiveFile)); err != nil {
		t.Fatal(err)
	}
	// A segment is stored under its own name with no manifest, so it is
	// never offered as a bundle.
	if listed, err := m.List(ctx, extension.BackupTarget{}); err != nil || !slices.Equal(listed, []string{"conduit-1"}) {
		t.Fatalf("listed = %v, %v", listed, err)
	}
	target := shipTarget{cfg: config.BackupDestination{Name: "offsite"}, d: m}
	if held, err := holdsBundle(ctx, target, source, "conduit-1", shipStallLimit); err != nil || !held {
		t.Fatalf("holds = %v, %v", held, err)
	}
	// A destination's own retention can expire a segment a bundle needs.
	delete(m.files, segments[0]+"/"+backup.ArchiveFile)
	if held, err := holdsBundle(ctx, target, source, "conduit-1", shipStallLimit); err != nil || held {
		t.Fatalf("a bundle missing a segment counted as held: %v, %v", held, err)
	}
	parent := t.TempDir()
	_, err = fetchBundle(ctx, m, extension.BackupTarget{}, "conduit-1", filepath.Join(parent, "fetched"), cli.RestoreOptions{ConfigPath: filepath.Join(t.TempDir(), "absent.json"), AcceptUnsigned: true})
	if err == nil || !strings.Contains(err.Error(), "missing at the destination") {
		t.Fatalf("fetch without its segment: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(parent, "fetched")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("published a bundle without its segment")
	}
	// The next shipment puts it back.
	if _, _, err := putBundle(ctx, target, source, "conduit-1", shipStallLimit); err != nil {
		t.Fatal(err)
	}
	if held, err := holdsBundle(ctx, target, source, "conduit-1", shipStallLimit); err != nil || !held {
		t.Fatalf("after reshipping: %v, %v", held, err)
	}
}

// Shipping a later scheduled bundle sends only the artifacts the destination
// lacks, and a fetch brings back the ones the bundle names.
func TestShipmentSendsOnlyNewArtifacts(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	c, store, err := openState(ctx, f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files, err := artifacts.Open(filepath.Join(f.state, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	attach := func(id, content string) {
		t.Helper()
		w, err := files.Create(ctx, "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
		ref, err := w.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			if err := tx.PutScope(domain.Scope{ID: domain.ID("ship-" + id), Name: "ship-" + id}, 0); err != nil {
				return err
			}
			e := domain.Event{ID: domain.ID("ship-" + id), Type: "fixture.seen", Source: "fixture", DeliveryID: "ship-" + id, OccurredAt: now, ReceivedAt: now}
			if _, _, err := tx.AcceptEvent(e); err != nil {
				return err
			}
			if err := tx.PutWork(domain.Work{ID: domain.ID("ship-" + id), Kind: "fixture.work", State: domain.WorkReady, ScopeID: domain.ID("ship-" + id)}, 0); err != nil {
				return err
			}
			return tx.PutExecution(domain.Execution{ID: domain.ID("ship-" + id), WorkID: domain.ID("ship-" + id), ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: e.ID, Artifacts: []domain.ArtifactRef{ref}}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	parent := t.TempDir()
	create := func(name string) string {
		t.Helper()
		bundle := filepath.Join(parent, name)
		if err := backup.CreateScheduled(ctx, bundle, nil, c.name, store, files, nil, "test", retention(time.Now)); err != nil {
			t.Fatal(err)
		}
		return bundle
	}
	objects := func(m *memoryDestination) int {
		n := 0
		for _, p := range m.order {
			if p == backup.ObjectFile {
				n++
			}
		}
		return n
	}
	m := newMemoryDestination()
	target := shipTarget{cfg: config.BackupDestination{Name: "offsite"}, d: m}
	attach("one", "first artifact")
	first := create("conduit-1")
	if _, _, err := putBundle(ctx, target, first, "conduit-1", shipStallLimit); err != nil {
		t.Fatal(err)
	}
	sent := objects(m)
	attach("two", "second artifact")
	second := create("conduit-2")
	if held, err := holdsBundle(ctx, target, second, "conduit-2", shipStallLimit); err != nil || held {
		t.Fatalf("holds = %v, %v", held, err)
	}
	if _, _, err := putBundle(ctx, target, second, "conduit-2", shipStallLimit); err != nil {
		t.Fatal(err)
	}
	if got := objects(m) - sent; got != 1 {
		t.Fatalf("the second shipment sent %d objects, want the 1 new one", got)
	}
	if listed, err := m.List(ctx, extension.BackupTarget{}); err != nil || !slices.Equal(listed, []string{"conduit-1", "conduit-2"}) {
		t.Fatalf("listed = %v, %v", listed, err)
	}
	options := cli.RestoreOptions{ConfigPath: filepath.Join(t.TempDir(), "absent.json"), AcceptUnsigned: true}
	fetched := filepath.Join(t.TempDir(), "fetched")
	info, err := fetchBundle(ctx, m, extension.BackupTarget{}, "conduit-2", fetched, options)
	if err != nil {
		t.Fatal(err)
	}
	if info.Artifacts < 2 {
		t.Fatalf("fetched %d artifacts", info.Artifacts)
	}
	shared, err := backup.SharedParts(second)
	if err != nil || len(shared) < 2 {
		t.Fatalf("shared = %v, %v", shared, err)
	}
	// A destination's own retention can expire an object a bundle needs.
	delete(m.files, shared[0].Name+"/"+shared[0].File)
	if held, err := holdsBundle(ctx, target, second, "conduit-2", shipStallLimit); err != nil || held {
		t.Fatalf("a bundle missing an object counted as held: %v, %v", held, err)
	}
	if _, err := fetchBundle(ctx, m, extension.BackupTarget{}, "conduit-2", filepath.Join(t.TempDir(), "fetched"), options); err == nil || !strings.Contains(err.Error(), "missing at the destination") {
		t.Fatalf("fetch without its object: %v", err)
	}
	if _, _, err := putBundle(ctx, target, second, "conduit-2", shipStallLimit); err != nil {
		t.Fatal(err)
	}
	if held, err := holdsBundle(ctx, target, second, "conduit-2", shipStallLimit); err != nil || !held {
		t.Fatalf("after reshipping: %v, %v", held, err)
	}
}

func TestFetchRefusesWhatADestinationCantMean(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"../escape", "artifacts/../../escape", "/etc/passwd", "private/operator.token", "artifacts//x"} {
		t.Run(path, func(t *testing.T) {
			m, public, name := shippedBundle(t)
			m.extra = []extension.BackupFile{{Path: path, Size: 1}}
			parent := t.TempDir()
			_, err := fetchBundle(ctx, m, extension.BackupTarget{}, name, filepath.Join(parent, "fetched"), cli.RestoreOptions{TrustKeys: []string{public}})
			if err == nil || !strings.Contains(err.Error(), "unexpected file") {
				t.Fatalf("err = %v", err)
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 0 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}
}

func TestBackupDestinationSettings(t *testing.T) {
	doc := func(body string) []byte {
		return fmt.Appendf(nil, `{"version":1,"settings":{"backup":{"destinations":%s}},"connections":[],"scopes":[{"id":"org","name":"org"}]}`, body)
	}
	parsed, err := config.ParseFile(doc(`[{"name":"offsite","type":"aws-s3","config":{"bucket":"b"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Settings.Backup.Destinations
	var c map[string]string
	if len(got) != 1 || got[0].Name != "offsite" || got[0].Type != "aws-s3" || json.Unmarshal(got[0].Config, &c) != nil || c["bucket"] != "b" {
		t.Fatalf("destinations = %+v", got)
	}
	if _, err := config.ParseFile(doc(`[{"name":"a","type":"x"},{"name":"a","type":"y"}]`)); err == nil {
		t.Fatal("a repeated name was accepted")
	}
}
