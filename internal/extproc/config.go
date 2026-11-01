package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
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
		return protocol.ConfigBaseResult{Generation: b.Generation, Hash: b.Hash, Handle: b.Handle, Text: string(b.Text), Folders: b.Folders}, nil
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
		if errors.Is(err, inproc.ErrConfigHandle) {
			return nil, configError(err)
		}
		if err != nil {
			return nil, invalid(err.Error())
		}
		return protocol.ConfigEditResult{Handle: e.Handle, Text: string(e.Text), Before: e.Before}, nil
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
		for _, d := range diags {
			out.Diagnostics = append(out.Diagnostics, protocol.ConfigDiagnostic{Path: d.Path, Code: d.Code, Message: d.Message})
		}
		return out, nil
	case "config.trace":
		var p protocol.ConfigTraceParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if p.Scope == "" || len(p.Scope) > maxConfigID || len(p.Sample) > maxConfigID {
			return nil, invalid("a scope's ID, and a sample's ID from the list a trace without one returns")
		}
		t, err := host.Trace(ctx, p.Handle, p.Scope, p.Sample)
		if errors.Is(err, inproc.ErrConfigHandle) {
			return nil, configError(err)
		}
		out := protocol.ConfigTraceResult{Sample: t.Sample,
			Effect: protocol.ConfigTraceEffect{Kind: t.Effect.Kind, WorkID: t.Effect.WorkID, Binding: t.Effect.Binding, Detail: t.Effect.Detail}}
		for _, x := range t.Samples {
			out.Samples = append(out.Samples, protocol.ConfigTraceSample{ID: x.ID, Title: x.Title})
		}
		for _, r := range t.Routes {
			out.Routes = append(out.Routes, protocol.ConfigTraceRoute{Route: r.Route, Handler: r.Handler, Binding: r.Binding, Verdict: r.Verdict, Reason: r.Reason})
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
	if errors.Is(err, inproc.ErrConfigHandle) {
		return protocol.Errorf(protocol.CodeNotFound, err.Error())
	}
	return protocol.Errorf(protocol.CodeInternal, err.Error())
}

// ConfigCandidate asks the package what a Work proposes (manifest
// config_candidate, permission config.read), bounded. A package that
// declares neither, or proposes nothing, has none: storage.ErrNotFound.
func (r *Remote) ConfigCandidate(ctx context.Context, w domain.Work) (extension.Candidate, error) {
	if !r.m.Provides.ConfigCandidate || !r.m.Allows(protocol.PermissionConfigRead) {
		return extension.Candidate{}, storage.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out protocol.ConfigCandidateResult
	if err := r.p.Call(ctx, "config.candidate", protocol.ConfigCandidateParams{Work: protocol.FromWork(w)}, &out); err != nil {
		return extension.Candidate{}, err
	}
	if !out.Found {
		return extension.Candidate{}, storage.ErrNotFound
	}
	return out.Candidate(), nil
}
