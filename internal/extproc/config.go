package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// The host's config service (permission config.read; the catalogue refuses
// the methods as undeclared without it). The host keeps config text behind
// handles and answers only copies with reference text redacted, so a
// package proposes edits to text it never holds. Nothing here applies a
// configuration.

const (
	// maxConfigPointer and maxConfigValue bound one proposed edit, as a
	// setup session's are.
	maxConfigPointer = 512
	maxConfigValue   = 64 << 10
	// maxConfigID bounds a trace's scope and sample IDs.
	maxConfigID = 200
)

// ConfigHost is the host's config service behind config.*: it keeps the
// config file's text behind handles and answers only copies with every
// secret_refs value replaced by "[redacted]", so a package never holds
// reference text. Nothing in it applies a configuration: only the operator
// does that, through POST /config/apply. Its errors are fixed, secret-free
// reasons.
type ConfigHost interface {
	// Base is the configuration now: the committed generation and its
	// hash, a handle on the config file's text, its redacted copy and the
	// IDs of the scopes that come from included folders.
	Base(ctx context.Context) (protocol.ConfigBaseResult, error)
	// Edit makes one RFC 6902 edit (add, replace or remove) to the text
	// behind handle, keeping its layout and key order, and holds the
	// result as a new candidate.
	Edit(ctx context.Context, handle, op, pointer string, value json.RawMessage) (protocol.ConfigEditResult, error)
	// Validate runs the side-effect-free validation POST /config/validate
	// runs on the text behind handle: the candidate's hash when it is
	// valid, its diagnostics when not.
	Validate(ctx context.Context, handle string) (hash string, diagnostics []protocol.ConfigDiagnostic, err error)
	// Trace runs the route trace for one of a scope's named samples
	// against the text behind handle, writing nothing. With no sample it
	// lists the scope's samples. An error is the trace's reason, answered
	// with what the trace got to (ConfigTraceResult.Error).
	Trace(ctx context.Context, handle, scope, sample string) (protocol.ConfigTraceResult, error)
}

// ErrConfigHandle refuses a config handle the host doesn't hold (one from
// before a restart, or long unused): read the base again.
var ErrConfigHandle = errors.New("the host no longer holds that configuration; read the base again")

func (s *Service) config(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	host := s.deps.Config
	if host == nil {
		return nil, protocol.Errorf(protocol.CodeUnsupported, "config")
	}
	switch method {
	case "config.base":
		b, err := host.Base(ctx)
		if err != nil {
			return nil, configError(err)
		}
		return b, nil
	case "config.edit":
		var p protocol.ConfigEditParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(p.Pointer, "/") || len(p.Pointer) > maxConfigPointer || strings.ContainsAny(p.Pointer, "\r\n") {
			return nil, invalid("pointer is a JSON pointer of at most 512 bytes")
		}
		switch p.Op {
		case "add", "replace":
			if len(p.Value) == 0 || len(p.Value) > maxConfigValue || !json.Valid(p.Value) {
				return nil, invalid(p.Op + " needs a JSON value of at most 64 KiB")
			}
		case "remove":
			if len(p.Value) > 0 {
				return nil, invalid("remove takes no value")
			}
		default:
			return nil, invalid("op is add, replace or remove")
		}
		e, err := host.Edit(ctx, p.Handle, p.Op, p.Pointer, p.Value)
		if errors.Is(err, ErrConfigHandle) {
			return nil, configError(err)
		}
		if err != nil {
			return nil, invalid(err.Error())
		}
		return e, nil
	case "config.validate":
		var p protocol.ConfigHandleParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		hash, diags, err := host.Validate(ctx, p.Handle)
		if err != nil {
			return nil, configError(err)
		}
		out := protocol.ConfigValidateResult{}
		if len(diags) == 0 {
			out.Hash = hash
		}
		out.Diagnostics = diags
		return out, nil
	case "config.trace":
		var p protocol.ConfigTraceParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if p.Scope == "" || len(p.Scope) > maxConfigID || len(p.Sample) > maxConfigID {
			return nil, invalid("a scope's ID, and a sample's ID from the list a trace without one returns")
		}
		out, err := host.Trace(ctx, p.Handle, p.Scope, p.Sample)
		if errors.Is(err, ErrConfigHandle) {
			return nil, configError(err)
		}
		if err != nil {
			// The host's trace reasons are fixed and secret-free.
			out.Error = err.Error()
		}
		return out, nil
	}
	return nil, protocol.Errorf(protocol.CodeUnsupported, method)
}

// configError maps the config service's failures: a handle the host no
// longer holds is not_found (read the base again); the host's other
// reasons are fixed and secret-free.
func configError(err error) error {
	if errors.Is(err, ErrConfigHandle) {
		return protocol.Errorf(protocol.CodeNotFound, err.Error())
	}
	return protocol.Errorf(protocol.CodeInternal, err.Error())
}

// candidateAttempts bounds how often ConfigCandidate asks again after a
// conflict.
const candidateAttempts = 5

// ConfigCandidate asks the package what a Work proposes (manifest
// config_candidate, permission config.read), bounded. A package that
// declares neither, or proposes nothing, has none: storage.ErrNotFound.
func (r *Remote) ConfigCandidate(ctx context.Context, w domain.Work) (extension.Candidate, error) {
	if !r.m.Provides.ConfigCandidate || !r.m.Allows(protocol.PermissionConfigRead) {
		return extension.Candidate{}, storage.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Reading the candidate may write the package's own state (a change
	// set moved onto a new generation), and a package's KV transaction
	// fails with a conflict rather than retrying when its other work wrote
	// the same keys at the same moment (the applied task straight after an
	// apply, say). The read changes nothing the operator decided, so ask
	// again rather than refuse it.
	var out protocol.ConfigCandidateResult
	for attempt := 0; ; attempt++ {
		out = protocol.ConfigCandidateResult{}
		err := r.p.Call(ctx, "config.candidate", protocol.ConfigCandidateParams{Work: protocol.FromWork(w)}, &out)
		if err == nil {
			break
		}
		if !errors.Is(err, storage.ErrConflict) || attempt == candidateAttempts-1 || ctx.Err() != nil {
			return extension.Candidate{}, err
		}
	}
	if !out.Found {
		return extension.Candidate{}, storage.ErrNotFound
	}
	return out.Candidate(), nil
}
