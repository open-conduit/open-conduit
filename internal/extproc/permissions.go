package extproc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// The extension-to-host methods a manifest permission gates (telemetry
// capture and setup probes). The catalogue already refuses them as
// undeclared without the permission; these check what is asked.

const (
	// maxTelemetryName and maxTelemetryProps bound one captured event.
	maxTelemetryName  = 128
	maxTelemetryProps = 64 << 10
	// probeTimeout bounds one probe run, as a setup session's does.
	probeTimeout = 30 * time.Second
	// maxProbeError bounds the reason a failed probe answers.
	maxProbeError = 300
)

// capture writes one named telemetry event for a configured scope of the
// extension, or for the machine ("").
func (s *Service) capture(ctx context.Context, p protocol.TelemetryEvent) error {
	if s.deps.Capture == nil {
		return protocol.Errorf(protocol.CodeUnsupported, "telemetry.capture")
	}
	if p.ScopeID != "" && !s.allowedScopes()[domain.ID(p.ScopeID)] {
		return unauthorized("scope is not configured for this extension")
	}
	if p.Name == "" || len(p.Name) > maxTelemetryName || strings.ContainsAny(p.Name, "\x00\n") {
		return protocol.Errorf(protocol.CodeInvalid, "an event name of at most 128 bytes")
	}
	if _, ok := p.Properties["scope"]; ok {
		return protocol.Errorf(protocol.CodeInvalid, "the scope goes in scope_id, not the properties")
	}
	if raw, err := json.Marshal(p.Properties); err != nil || len(raw) > maxTelemetryProps {
		return protocol.Errorf(protocol.CodeInvalid, "properties of at most 64 KiB")
	}
	at := p.At
	if at.IsZero() {
		at = s.deps.Now()
	}
	return s.deps.Capture(ctx, p.ScopeID, p.Name, at, p.Properties)
}

// actionList lists what every provider offers on a target in a scope the
// extension is configured for.
func (s *Service) actionList(ctx context.Context, p protocol.ActionListParams) (protocol.ActionListResult, error) {
	if s.deps.Actions == nil {
		return protocol.ActionListResult{}, protocol.Errorf(protocol.CodeUnsupported, "action.list")
	}
	if !s.allowedScopes()[domain.ID(p.ScopeID)] {
		return protocol.ActionListResult{}, unauthorized("scope is not configured for this extension")
	}
	offers, err := s.deps.Actions(ctx, s.name, domain.ID(p.ScopeID), domain.ID(p.WorkID))
	if err != nil {
		return protocol.ActionListResult{}, err
	}
	out := protocol.ActionListResult{Actions: []protocol.ActionOffer{}}
	for _, o := range offers {
		out.Actions = append(out.Actions, protocol.ActionOffer{Provider: o.Provider, ID: o.ID, Label: o.Label, Prompt: o.Prompt, Choices: o.Choices, Detail: o.Detail, Awaited: o.Awaited})
	}
	return out, nil
}

// requestList lists the requests every provider reports on Work in a scope
// the extension is configured for.
func (s *Service) requestList(ctx context.Context, p protocol.RequestListParams) (protocol.RequestListResult, error) {
	if s.deps.Requests == nil {
		return protocol.RequestListResult{}, protocol.Errorf(protocol.CodeUnsupported, "request.list")
	}
	if !s.allowedScopes()[domain.ID(p.ScopeID)] {
		return protocol.RequestListResult{}, unauthorized("scope is not configured for this extension")
	}
	offers, err := s.deps.Requests(ctx, s.name, domain.ID(p.ScopeID), domain.ID(p.WorkID))
	if err != nil {
		return protocol.RequestListResult{}, err
	}
	out := protocol.RequestListResult{Requests: []protocol.RequestOffer{}}
	for _, o := range offers {
		out.Requests = append(out.Requests, protocol.RequestOffer{Provider: o.Provider, Request: protocol.FromRequest(o.Request)})
	}
	return out, nil
}

// actionSubmit invokes an action for a person the extension identified on
// its own surface, in a scope it is configured for, as a caller of its own
// kind.
func (s *Service) actionSubmit(ctx context.Context, p protocol.ActionSubmitParams) (protocol.ActionSubmitResult, error) {
	if s.deps.Act == nil {
		return protocol.ActionSubmitResult{}, protocol.Errorf(protocol.CodeUnsupported, "action.submit")
	}
	if !s.allowedScopes()[domain.ID(p.ScopeID)] {
		return protocol.ActionSubmitResult{}, unauthorized("scope is not configured for this extension")
	}
	r, err := s.deps.Act(ctx, s.name, extension.ActRequest{Scope: domain.ID(p.ScopeID), Work: domain.ID(p.WorkID), Provider: p.Provider, Action: p.Action, Input: p.Input, Caller: p.Caller, Key: p.Key})
	if err != nil {
		return protocol.ActionSubmitResult{}, err
	}
	return protocol.ActionSubmitResult{Event: protocol.FromAccepted(r.Event), Added: r.Added, Message: r.Message}, nil
}

func (s *Service) probeList() (protocol.ProbeListResult, error) {
	if s.deps.Probes == nil {
		return protocol.ProbeListResult{}, protocol.Errorf(protocol.CodeUnsupported, "probes")
	}
	out := protocol.ProbeListResult{Probes: []protocol.ProbeInfo{}}
	for name, p := range s.deps.Probes() {
		info := protocol.ProbeInfo{Name: name, Summary: p.Summary}
		for _, a := range p.Args {
			info.Args = append(info.Args, protocol.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required, Connection: a.Connection})
		}
		out.Probes = append(out.Probes, info)
	}
	sort.Slice(out.Probes, func(i, j int) bool { return out.Probes[i].Name < out.Probes[j].Name })
	return out, nil
}

// probeRun runs one probe with arguments it declares. A probe that fails
// answers its reason, bounded, as a setup session shows it.
func (s *Service) probeRun(ctx context.Context, p protocol.ProbeRunParams) (protocol.ProbeRunResult, error) {
	if s.deps.Probes == nil {
		return protocol.ProbeRunResult{}, protocol.Errorf(protocol.CodeUnsupported, "probes")
	}
	probe, ok := s.deps.Probes()[p.Name]
	if !ok {
		return protocol.ProbeRunResult{}, protocol.Errorf(protocol.CodeNotFound, "no such probe")
	}
	declared := map[string]bool{}
	for _, a := range probe.Args {
		declared[a.Name] = true
		if a.Required && p.Args[a.Name] == "" {
			return protocol.ProbeRunResult{}, protocol.Errorf(protocol.CodeInvalid, "argument "+a.Name+" is required")
		}
	}
	for name := range p.Args {
		if !declared[name] {
			return protocol.ProbeRunResult{}, protocol.Errorf(protocol.CodeInvalid, "the probe takes no argument "+name)
		}
	}
	for _, a := range probe.Args {
		if !a.Connection || p.Args[a.Name] == "" {
			continue
		}
		// The caller speaks only for a scope it is configured in; the
		// probe's owner then checks that scope grants the connection.
		if p.ScopeID == "" {
			return protocol.ProbeRunResult{}, invalid("a probe through a connection needs scope_id")
		}
		if !s.allowedScopes()[domain.ID(p.ScopeID)] {
			return protocol.ProbeRunResult{}, unauthorized("scope is not configured for this extension")
		}
		ctx = context.WithValue(ctx, probeScopeKey{}, domain.ID(p.ScopeID))
	}
	cx, cancel := context.WithTimeout(ctx, probeTimeout)
	res, err := probe.Run(cx, p.Args)
	cancel()
	if err != nil {
		msg := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "the probe timed out"
		}
		if len(msg) > maxProbeError {
			msg = msg[:maxProbeError]
		}
		return protocol.ProbeRunResult{Command: res.Command, Error: msg}, nil
	}
	out := protocol.ProbeRunResult{Command: res.Command}
	if res.Output != nil {
		raw, err := json.Marshal(res.Output)
		if err != nil {
			return protocol.ProbeRunResult{}, err
		}
		out.Output = raw
	}
	return out, nil
}

// probeScopeKey carries the scope probe.run checked to the probe's owner.
type probeScopeKey struct{}

// probeGrant is one running probe's access to one connection's secrets.
type probeGrant struct {
	scope      domain.ID
	connection string
}

// errProbeConnection is a probe's fixed reason for a connection it can't
// use, the same whether the connection is missing or not granted, so a
// probe can't be used to find out which connections exist.
var errProbeConnection = errors.New("this session's scope isn't authorized for that connection: add it to the scope's authorization.connections and apply")

// grantProbeConnection fills params for a run through connection, when the
// scope probe.run checked grants it and it is of a type this package
// declares, and opens a grant on its secrets until the returned func runs.
func (s *Service) grantProbeConnection(ctx context.Context, params *protocol.ProbeRunParams, connection string) (func(), error) {
	scope, ok := ctx.Value(probeScopeKey{}).(domain.ID)
	if !ok || s.deps.ProbeConnection == nil {
		return nil, errProbeConnection
	}
	entry, ok := s.deps.ProbeConnection(scope, connection)
	if !ok {
		return nil, errProbeConnection
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw[:])
	s.mu.Lock()
	if s.probeGrants == nil {
		s.probeGrants = map[string]probeGrant{}
	}
	s.probeGrants[id] = probeGrant{scope: scope, connection: entry.ID}
	s.mu.Unlock()
	params.ScopeID, params.Connection, params.Grant = string(scope), &entry, id
	return func() {
		s.mu.Lock()
		delete(s.probeGrants, id)
		s.mu.Unlock()
	}, nil
}

// probeSecrets answers secret.resolve under a running probe's grant.
func (s *Service) probeSecrets(ctx context.Context, p protocol.SecretResolveParams) (protocol.SecretResolveResult, error) {
	s.mu.Lock()
	g, ok := s.probeGrants[p.ProbeGrant]
	s.mu.Unlock()
	if !ok || string(g.scope) != p.ScopeID || g.connection != p.Connection || p.Binding != "" {
		return protocol.SecretResolveResult{}, unauthorized("no running probe holds this connection")
	}
	if s.deps.ProbeSecrets == nil {
		return protocol.SecretResolveResult{}, protocol.Errorf(protocol.CodeUnsupported, "probe secrets")
	}
	values, err := s.deps.ProbeSecrets(ctx, g.scope, g.connection, p.Names)
	if err != nil {
		return protocol.SecretResolveResult{}, err
	}
	return protocol.SecretResolveResult{Values: values}, nil
}

// Probes are the package's declared setup probes (provides.probes), by
// their own names, each run through probe.serve. A probe that ran and
// failed answers its reason, which comes back as the probe's error.
func (r *Remote) Probes() []extension.SetupProbe {
	var out []extension.SetupProbe
	for _, info := range r.m.Provides.Probes {
		name := info.Name
		p := extension.SetupProbe{Name: name, Summary: info.Summary}
		for _, a := range info.Args {
			p.Args = append(p.Args, extension.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required, Connection: a.Connection})
		}
		p.Run = func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
			params := protocol.ProbeRunParams{Name: name, Args: args}
			for _, a := range info.Args {
				if !a.Connection || args[a.Name] == "" {
					continue
				}
				closeGrant, err := r.s.grantProbeConnection(ctx, &params, args[a.Name])
				if err != nil {
					return extension.ProbeResult{}, err
				}
				defer closeGrant()
			}
			var res protocol.ProbeRunResult
			if err := r.p.Call(ctx, "probe.serve", params, &res); err != nil {
				return extension.ProbeResult{}, err
			}
			if res.Error != "" {
				return extension.ProbeResult{}, errors.New(res.Error)
			}
			var output any
			if len(res.Output) > 0 {
				if err := json.Unmarshal(res.Output, &output); err != nil {
					return extension.ProbeResult{}, err
				}
			}
			return extension.ProbeResult{Command: res.Command, Output: output}, nil
		}
		out = append(out, p)
	}
	return out
}
