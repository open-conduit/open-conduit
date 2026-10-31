package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
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

func (s *Service) probeList() (protocol.ProbeListResult, error) {
	if s.deps.Probes == nil {
		return protocol.ProbeListResult{}, protocol.Errorf(protocol.CodeUnsupported, "probes")
	}
	out := protocol.ProbeListResult{Probes: []protocol.ProbeInfo{}}
	for name, p := range s.deps.Probes() {
		info := protocol.ProbeInfo{Name: name, Summary: p.Summary}
		for _, a := range p.Args {
			info.Args = append(info.Args, protocol.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required})
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
