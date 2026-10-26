package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
)

// Event payload schema evolution (07-02, provisional).
//
// A payload schema identifier is <base>.v<N>: a dotted base in the
// extension's own namespace and a positive integer version. The manifest
// declares each base with the versions the running extension can read; the
// highest is the one it writes. Persisted envelopes are immutable: the host
// never rewrites a stored Event, so an old Event reaches an extension exactly
// as it was accepted, and the extension upcasts readable older versions
// itself (sdk.Upcaster). The host gates the rest deterministically through
// PayloadStatus, before any call, so an unreadable payload never depends on
// what a process happens to do with it.

// SchemaDecl declares one payload schema base and its readable versions,
// strictly increasing; the last is the version the extension writes.
type SchemaDecl struct {
	Name     string `json:"name"`
	Versions []int  `json:"versions"`
}

// SchemaStatus classifies a payload against a manifest.
type SchemaStatus string

const (
	// SchemaForeign: no payload, or a schema outside the extension's
	// namespace (core, another extension). The host passes it verbatim and
	// applies no evolution rule; reading it is a data contract.
	SchemaForeign SchemaStatus = "foreign"
	// SchemaCurrent: the version the extension writes.
	SchemaCurrent SchemaStatus = "current"
	// SchemaUpcast: an older version the extension still reads; delivered
	// verbatim for the extension to upcast.
	SchemaUpcast SchemaStatus = "upcast"
	// SchemaRetired: an own version older than every readable version (the
	// extension dropped its upcaster).
	SchemaRetired SchemaStatus = "retired"
	// SchemaNewer: an own version newer than the one the extension writes,
	// persisted by a newer release before a downgrade.
	SchemaNewer SchemaStatus = "newer"
	// SchemaUndeclared: an own-namespace schema whose base is not declared,
	// or an identifier without a version suffix.
	SchemaUndeclared SchemaStatus = "undeclared"
)

// Readable reports whether the extension can be asked to decide on a
// payload of this status.
func (s SchemaStatus) Readable() bool {
	return s == SchemaForeign || s == SchemaCurrent || s == SchemaUpcast
}

var versionSegment = regexp.MustCompile(`^v[1-9][0-9]{0,8}$`)

// ParseSchemaID splits <base>.v<N>.
func ParseSchemaID(id string) (base string, version int, ok bool) {
	i := strings.LastIndex(id, ".")
	if i <= 0 || !versionSegment.MatchString(id[i+1:]) {
		return "", 0, false
	}
	v, err := strconv.Atoi(id[i+2:])
	if err != nil {
		return "", 0, false
	}
	return id[:i], v, true
}

// SchemaID formats <base>.v<N>.
func SchemaID(base string, version int) string { return base + ".v" + strconv.Itoa(version) }

// PayloadStatus classifies p against the manifest's schema declarations.
func (m Manifest) PayloadStatus(p domain.Payload) SchemaStatus {
	if p.Schema == "" || !Namespace(m.Name, p.Schema) {
		return SchemaForeign
	}
	base, v, ok := ParseSchemaID(p.Schema)
	if !ok {
		return SchemaUndeclared
	}
	for _, d := range m.Provides.Schemas {
		if d.Name != base {
			continue
		}
		current := d.Versions[len(d.Versions)-1]
		switch {
		case v == current:
			return SchemaCurrent
		case v > current:
			return SchemaNewer
		}
		for _, r := range d.Versions {
			if r == v {
				return SchemaUpcast
			}
		}
		return SchemaRetired
	}
	return SchemaUndeclared
}

// ErrSchema wraps every payload schema refusal.
var ErrSchema = errors.New("protocol: unreadable or unwritable payload schema")

// CheckWrite enforces the write rule: a payload an extension writes (an
// ingress observation or event, an event.accept, an emission) is absent or
// carries the current version of one of its own declared schemas.
func (m Manifest) CheckWrite(p domain.Payload) error {
	if p.Schema == "" && len(p.Data) == 0 {
		return nil
	}
	if !Namespace(m.Name, p.Schema) {
		return fmt.Errorf("%w: %q is outside namespace %q", ErrSchema, p.Schema, m.Name)
	}
	if s := m.PayloadStatus(p); s != SchemaCurrent {
		return fmt.Errorf("%w: %q is %s", ErrSchema, p.Schema, s)
	}
	return nil
}

// KV layout evolution (07-02, provisional). The host records, per extension,
// the KV layout version the extension last reported from a successful
// configure. Starting a manifest that declares an older kv_version than the
// recorded one is refused (no downgrade); a newer one is told the stored
// version in configure and migrates its own namespaces with ordinary
// precondition-guarded kv.commit batches before answering.

// ErrKVDowngrade refuses a manifest older than its recorded KV layout.
var ErrKVDowngrade = errors.New("protocol: extension KV layout is newer than the manifest (downgrade refused)")

// CheckKV decides what a start does with the recorded KV layout version.
// It returns whether configure must ask for a migration.
func CheckKV(stored, declared int) (migrate bool, err error) {
	switch {
	case stored > declared:
		return false, fmt.Errorf("%w: stored %d, manifest %d", ErrKVDowngrade, stored, declared)
	case stored < declared:
		return true, nil
	}
	return false, nil
}
