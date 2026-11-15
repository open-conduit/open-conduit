package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/open-conduit/open-conduit/domain"
)

// Entry operations are toolbelt operations an extension derives from the
// operator's config entries rather than declaring them in its manifest: an
// MCP bridge whose server's tools become operations, or a command entry
// that runs a script. The entry's config is authoritative: the extension
// reads the operations it declares from the config alone, without starting
// anything, so config validation resolves grants offline, and the host
// registers exactly those operations for each generation. An operation no
// entry declares can't be granted or called, and removing an entry removes
// its operations. The extension checks the declared list against what it
// fronts when it starts and refuses a call to anything that has drifted.
//
// Each is named <registration>.<entry>.<name> (EntryOperationName), in the
// registration's namespace, so the usual OwnsOperation and conflict checks
// apply.

// EntryOperationDecl is one operation an entry's config declares: its
// name within the entry (EntryOperationSegment), what it does (Summary,
// one line), when and how to use it (Guidance, Markdown) and the JSON
// Schema of its input (Input, an object). Toolset optionally names a
// toolset within the entry the operation joins, which config grants as
// "@<registration>.<entry>.<toolset>".
type EntryOperationDecl struct {
	Name     string          `json:"name"`
	Summary  string          `json:"summary,omitempty"`
	Guidance string          `json:"guidance,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Toolset  string          `json:"toolset,omitempty"`
}

// EntryOperation is an entry operation as a Registration carries it: the
// entry that declares it, its declaration, and the scopes of the
// generation in which that entry is in effect. The toolbelt refuses a call
// from any other scope as undeclared.
type EntryOperation struct {
	Entry string
	EntryOperationDecl
	Scopes []domain.ID
}

// EntryCall is a call of an entry operation as its EntryHandler receives
// it: the entry and the operation's name within it, and the input.
type EntryCall struct {
	Entry, Operation string
	Input            domain.Payload
}

// EntryHandler serves a registration's entry operations. It receives
// validated authority, as Operation.Handler does, and must be idempotent on
// the call's request ID (CallRequestID) in the same way.
type EntryHandler func(context.Context, Authority, EntryCall) (domain.Payload, error)

var entrySegment = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// EntryOperationSegment reports whether s may name an entry or an entry
// operation within one: a lower-case letter, then up to 62 lower-case
// letters, digits, hyphens or underscores, with no dot, so
// <registration>.<entry>.<name> splits one way only.
func EntryOperationSegment(s string) bool { return entrySegment.MatchString(s) }

// EntryOperationName is the toolbelt name of the operation op that the
// entry of registration owner declares.
func EntryOperationName(owner, entry, op string) string { return owner + "." + entry + "." + op }

// CheckEntryOperationDecl reports what is wrong with d as an operation an
// entry declares: its name, its toolset's name, its summary, guidance or
// input schema.
func CheckEntryOperationDecl(d EntryOperationDecl) error {
	if !EntryOperationSegment(d.Name) {
		return fmt.Errorf("%w: entry operation %q must be a lower-case letter then letters, digits, - or _", ErrInvalid, d.Name)
	}
	if d.Toolset != "" && !EntryOperationSegment(d.Toolset) {
		return fmt.Errorf("%w: entry operation %q: toolset %q must be a lower-case letter then letters, digits, - or _", ErrInvalid, d.Name, d.Toolset)
	}
	if len(d.Guidance) > MaxGuidance {
		return fmt.Errorf("%w: entry operation %q: guidance is longer than %d bytes", ErrInvalid, d.Name, MaxGuidance)
	}
	if err := ValidateOperationDoc(d.Summary, d.Input); err != nil {
		return fmt.Errorf("entry operation %q: %w", d.Name, err)
	}
	return nil
}

// ExpandEntryOperations are r's entry operations as operations, each named
// <r.Name>.<entry>.<name> (EntryOperationName) and served by r's
// EntryHandler, with the scopes each may be called from by name. A
// toolbelt registers them with r's other operations, under the same
// namespace and conflict checks, and refuses a call of one from a scope it
// isn't listed for as ErrUndeclared. It refuses a malformed entry or
// operation name, or entry operations without a handler, as ErrInvalid,
// and a name declared twice as ErrConflict.
func ExpandEntryOperations(r Registration) ([]Operation, map[string][]domain.ID, error) {
	if len(r.EntryOperations) == 0 {
		return nil, nil, nil
	}
	if r.EntryHandler == nil {
		return nil, nil, fmt.Errorf("%w: extension %q registers entry operations without an entry handler", ErrInvalid, r.Name)
	}
	var ops []Operation
	scopes := map[string][]domain.ID{}
	for _, e := range r.EntryOperations {
		if !EntryOperationSegment(e.Entry) {
			return nil, nil, fmt.Errorf("%w: entry %q of extension %q must be a lower-case letter then letters, digits, - or _", ErrInvalid, e.Entry, r.Name)
		}
		if err := CheckEntryOperationDecl(e.EntryOperationDecl); err != nil {
			return nil, nil, err
		}
		name := EntryOperationName(r.Name, e.Entry, e.Name)
		if _, ok := scopes[name]; ok {
			return nil, nil, fmt.Errorf("%w: operation %q is declared twice by %s", ErrConflict, name, r.Name)
		}
		scopes[name] = slices.Clone(e.Scopes)
		entry, local, handler := e.Entry, e.Name, r.EntryHandler
		ops = append(ops, Operation{Name: name, Summary: e.Summary, Guidance: e.Guidance, Input: e.Input,
			Handler: func(ctx context.Context, a Authority, in domain.Payload) (domain.Payload, error) {
				return handler(ctx, a, EntryCall{Entry: entry, Operation: local, Input: in})
			}})
	}
	return ops, scopes, nil
}
