// Package notes is the example external extension of protocol version 1
// (07-02). It depends only on the public contracts and protocol/sdk, and
// the same Go code is served in-process (for the shared conformance suites)
// and as a subprocess (cmd/notes-extension), so the suites compare like with
// like.
//
// It models a small note service exported as a JSON file named by each
// scope entry's config: ingress (push of a note change or deletion,
// paged reconcile), a route creating one Work per note, a lookup operation
// over its KV index, an emission with committed records, a recurrence, an
// index task that derives events with event.accept, and three releases that
// exercise payload schema evolution and a KV migration:
//
//	release 1: writes notes.note.v1 {"id","body"};   reads [1];   kv_version 1
//	release 2: writes notes.note.v2 {"id","text"};   reads [1,2]; kv_version 2
//	release 3: writes notes.note.v3 {+"words"};      reads [2,3]; kv_version 2
//
// Observation version and delivery identities include the payload schema
// version (the documented rule), so a release that changes the payload
// re-observes each note once instead of conflicting with its canonical
// identity; StableIdentity breaks the rule for the negative test.
//
// The "fixture" profile serves the names the shared toolbelt suite uses
// (fixture.echo, fixture.wait, fixture.changed.v1).
package notes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

const (
	Name       = "notes"
	Changed    = "notes.note.changed"
	Deleted    = "notes.note.deleted"
	Indexed    = "notes.note.indexed"
	Annotated  = "notes.note.annotated"
	Kind       = "notes.note"
	Schema     = "notes.note"
	IndexNS    = "notes.index"
	Recurrence = "notes.daily"
	// TelemetryTask records a notes_pass telemetry event for each scope
	// and the machine, and writes the host's probes, and the output of
	// ProbeName run with its "say" argument, to probes.json in the state
	// directory.
	TelemetryTask = "notes-telemetry"
	ProbeName     = "host.echo"
	PassEvent     = "notes_pass"
)

// Options select the release and test behaviour of one process.
type Options struct {
	Release        int
	StableIdentity bool
	Fixture        bool
	// StateDir returns where faults.json is read (the process's state
	// directory, known after initialize); nil disables faults.
	StateDir func() string
}

func schemaVersions(release int) []int {
	switch release {
	case 1:
		return []int{1}
	case 2:
		return []int{1, 2}
	}
	return []int{2, 3}
}

func kvVersion(release int) int { return min(release, 2) }

// Manifest returns the manifest of a release (or the fixture profile).
func Manifest(o Options) protocol.Manifest {
	if o.Fixture {
		return protocol.Manifest{Protocol: []int{protocol.Version}, Name: "fixture", Version: "1.0.0", Command: []string{"notes-extension", "--fixture"}, Provides: protocol.Provides{
			Families:         []protocol.Family{{Type: "fixture.changed.v1", ReDerivable: true}},
			Operations:       []protocol.OperationDecl{{Name: "fixture.echo", Kind: protocol.OperationHandler}, {Name: "fixture.wait", Kind: protocol.OperationWait}, {Name: "fixture.report", Kind: protocol.OperationEmit, Commit: true}, {Name: "fixture.connection", Kind: protocol.OperationHandler}},
			DependencyFilter: true,
		}}
	}
	cmd := []string{"notes-extension", "--release", strconv.Itoa(o.Release)}
	if o.StableIdentity {
		cmd = append(cmd, "--stable-identity")
	}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: fmt.Sprintf("0.%d.0", o.Release), Summary: "Example external extension: a note service.", Command: cmd, KVVersion: kvVersion(o.Release),
		// It mirrors the host's telemetry events to sink.log, records one
		// per indexing pass, and runs the host's probes (TelemetryTask).
		Permissions: []string{protocol.PermissionTelemetrySink, protocol.PermissionTelemetryCapture, protocol.PermissionProbes},
		Provides: protocol.Provides{
			// An entry's service file and labels are safe to show the operator.
			Extensions: []protocol.ExtensionDecl{{Type: "notes", Ingress: true,
				Settings: []protocol.SettingDecl{{Field: "service", Type: protocol.SettingString}, {Field: "watch", Type: protocol.SettingBoolean, Default: json.RawMessage(`true`)}},
				Describe: &protocol.DescribeDecl{List: "labels", Name: "name", Detail: "note"}}},
			Families: []protocol.Family{
				{Type: Changed, ReDerivable: true},
				{Type: Deleted, Loss: "Deletions arrive only by push; one missed while push is down is never observed."},
				{Type: Indexed, ReDerivable: true},
				{Type: Annotated, ReDerivable: true},
			},
			Schemas:     []protocol.SchemaDecl{{Name: Schema, Versions: schemaVersions(o.Release)}},
			Routes:      []protocol.RouteDecl{{Handler: "notes"}},
			WorkKinds:   []protocol.WorkKindDecl{{Kind: Kind, Trigger: true}},
			Recurrences: []string{Recurrence},
			Operations:  []protocol.OperationDecl{{Name: "notes.lookup", Kind: protocol.OperationHandler}, {Name: "notes.annotate", Kind: protocol.OperationEmit, Commit: true}},
			Tasks:       []protocol.TaskDecl{{Name: "notes-index", IntervalSeconds: 60}, {Name: "notes-derive", IntervalSeconds: 60}, {Name: TelemetryTask, IntervalSeconds: 60}},
			Checks:      []protocol.CheckDecl{{Name: "notes"}},
			Render:      true,
			// 07-03 surfaces.
			Webhooks:  true,
			Resources: []protocol.ResourceDecl{{Type: Scratch, Modes: []string{extension.ResourceWork}}},
			Exporters: true,
			Annotate:  true,
			// A run of Work in a notes scope gets NOTES_WORK, the Work it runs,
			// and NOTES_CONNECTION, the connection its binding names.
			RunEnvironment: true,
			Actions:        true,
			Panels:         true,
			Setup:          &protocol.SetupDecl{Type: Name, Summary: "add a scope watching a note service file", Usage: "[--service FILE]"},
		}}
}

// Note is one item of the note service.
type Note struct {
	ID   string    `json:"id"`
	Rev  string    `json:"rev"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// ServiceFile is the note service's export: the reconcile source.
type ServiceFile struct {
	Notes []Note `json:"notes"`
}

// Ingress is the note service adapter.
type Ingress struct {
	Service        string
	Scope          string
	Release        int
	StableIdentity bool
}

func (i *Ingress) Spec() extension.IngressSpec {
	return extension.IngressSpec{Families: []extension.EventFamily{{Type: Changed, ReDerivable: true}, {Type: Deleted, Loss: "Deletions arrive only by push; one missed while push is down is never observed."}}}
}

func (i *Ingress) version() int { return schemaVersions(i.Release)[len(schemaVersions(i.Release))-1] }

// Payload encodes a note in the release's current payload version.
func Payload(release int, n Note) domain.Payload {
	v := schemaVersions(release)[len(schemaVersions(release))-1]
	var data []byte
	switch v {
	case 1:
		data, _ = json.Marshal(map[string]string{"id": n.ID, "body": n.Text})
	case 2:
		data, _ = json.Marshal(map[string]string{"id": n.ID, "text": n.Text})
	default:
		data, _ = json.Marshal(map[string]any{"id": n.ID, "text": n.Text, "words": len(strings.Fields(n.Text))})
	}
	return domain.Payload{Schema: protocol.SchemaID(Schema, v), Data: data}
}

func (i *Ingress) observe(source extension.Source, n Note) extension.Observation {
	version := n.Rev
	if !i.StableIdentity {
		version = n.Rev + ".s" + strconv.Itoa(i.version())
	}
	key := domain.CorrelationKey{Source: Name, ExternalID: n.ID}
	return extension.Observation{ID: n.ID, Version: version, Event: domain.Event{ID: domain.ID("notes/" + n.ID + "/" + version), Type: Changed, Source: source.Namespace, DeliveryID: n.ID + "/" + version, OccurredAt: n.At, ReceivedAt: n.At, CorrelationKey: &key, Data: Payload(i.Release, n)}}
}

type delivery struct {
	Note    *Note `json:"note,omitempty"`
	Deleted *struct {
		ID string    `json:"id"`
		At time.Time `json:"at"`
	} `json:"deleted,omitempty"`
}

func (i *Ingress) Push(ctx context.Context, source extension.Source, d extension.Delivery) (extension.PushResult, error) {
	if err := ctx.Err(); err != nil {
		return extension.PushResult{}, err
	}
	var in delivery
	if err := json.Unmarshal(d.Body, &in); err != nil {
		return extension.PushResult{}, extension.ErrInvalid
	}
	switch {
	case in.Note != nil:
		return extension.PushResult{Observations: []extension.Observation{i.observe(source, *in.Note)}}, nil
	case in.Deleted != nil:
		key := domain.CorrelationKey{Source: Name, ExternalID: in.Deleted.ID}
		return extension.PushResult{Events: []domain.Event{{ID: domain.ID("notes/deleted/" + in.Deleted.ID), Type: Deleted, Source: source.Namespace, DeliveryID: "deleted/" + in.Deleted.ID, OccurredAt: in.Deleted.At, ReceivedAt: in.Deleted.At, CorrelationKey: &key}}}, nil
	}
	return extension.PushResult{}, extension.ErrInvalid
}

// Reconcile pages the export one note at a time. The ScanID is a digest of
// the export, so a generation is a stable snapshot.
func (i *Ingress) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return extension.Snapshot{}, err
	}
	raw, err := os.ReadFile(i.Service)
	if err != nil {
		return extension.Snapshot{}, err
	}
	var file ServiceFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return extension.Snapshot{}, extension.ErrInvalid
	}
	sort.Slice(file.Notes, func(a, b int) bool { return file.Notes[a].ID < file.Notes[b].ID })
	sum := sha256.Sum256(raw)
	scan := s.ScanID
	if scan == "" {
		scan = hex.EncodeToString(sum[:8])
	}
	at := 0
	if s.Cursor != "" {
		if at, err = strconv.Atoi(s.Cursor); err != nil || at < 0 || at > len(file.Notes) {
			return extension.Snapshot{}, extension.ErrInvalid
		}
	}
	page := extension.Snapshot{ScanID: scan, Checkpoint: "page-" + strconv.Itoa(at)}
	if at < len(file.Notes) {
		page.Observations = []extension.Observation{i.observe(s.Source, file.Notes[at])}
	}
	if at+1 < len(file.Notes) {
		page.NextCursor = strconv.Itoa(at + 1)
	} else {
		page.Complete = true
	}
	return page, nil
}

// Upcasters lift older payload versions to the current one.
func Upcasters() map[string]sdk.Upcaster {
	return map[string]sdk.Upcaster{Schema: func(from int, data json.RawMessage) (json.RawMessage, error) {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		switch from {
		case 1:
			m["text"] = m["body"]
			delete(m, "body")
		case 2:
			text, _ := m["text"].(string)
			m["words"] = len(strings.Fields(text))
		default:
			return nil, errors.New("no upcast")
		}
		return json.Marshal(m)
	}}
}

// Text reads a note payload of the current version.
func Text(p domain.Payload) string {
	var m struct {
		Text string `json:"text"`
		Body string `json:"body"`
	}
	_ = json.Unmarshal(p.Data, &m)
	if m.Text != "" {
		return m.Text
	}
	return m.Body
}

// Fault is a one-shot test fault read from <state>/faults.json.
type Fault struct {
	Method string `json:"method"`
	// Mode: exit (crash), partial (half a response line, then crash),
	// hang (never answer), reenter (make a host callback from an
	// in-transaction method).
	Mode string `json:"mode"`
}

var faultMu sync.Mutex

// TakeFault consumes the fault for method, if one is armed.
func TakeFault(dir func() string, method string) (Fault, bool) {
	if dir == nil || dir() == "" {
		return Fault{}, false
	}
	stateDir := dir()
	faultMu.Lock()
	defer faultMu.Unlock()
	path := filepath.Join(stateDir, "faults.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fault{}, false
	}
	var f Fault
	if json.Unmarshal(raw, &f) != nil || f.Method != method {
		return Fault{}, false
	}
	_ = os.Remove(path)
	return f, true
}

// ArmFault writes a one-shot fault for the next call of method.
func ArmFault(stateDir string, f Fault) error {
	raw, _ := json.Marshal(f)
	return os.WriteFile(filepath.Join(stateDir, "faults.json"), raw, 0o600)
}
