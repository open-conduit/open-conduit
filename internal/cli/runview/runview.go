// Package runview renders unified run history records as plain text. The
// same row, detail and log selection serve every executor; nothing here
// branches on executor, work kind or domain payloads.
package runview

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// LogArtifact selects the run's log: the first text artifact, else the first.
func LogArtifact(r client.Run) (client.Artifact, bool) {
	for _, a := range r.Artifacts {
		if strings.HasPrefix(a.MediaType, "text/") {
			return a, true
		}
	}
	if len(r.Artifacts) > 0 {
		return r.Artifacts[0], true
	}
	return client.Artifact{}, false
}

// Fit truncates or pads to n runes.
func Fit(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		if n <= 1 {
			return string(r[:n])
		}
		return string(r[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(r))
}

// ShortTime renders an RFC 3339 time in local time, month to second.
func ShortTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.Local().Format("01-02 15:04:05")
}

// Duration renders an attempt's elapsed time; live attempts count from start.
func Duration(r client.Run, now time.Time) string {
	if r.DurationMS > 0 || r.FinishedAt != "" {
		return (time.Duration(r.DurationMS) * time.Millisecond).Truncate(time.Millisecond).String()
	}
	if t, err := time.Parse(time.RFC3339Nano, r.StartedAt); err == nil {
		return now.Sub(t).Truncate(time.Second).String() + "…"
	}
	return ""
}

// Outcome renders the outcome kind and summary, or the live state.
func Outcome(r client.Run) string {
	if r.Outcome == nil {
		if r.WaitID != "" && r.State == "parked" {
			return "waiting on " + r.WaitID
		}
		return ""
	}
	if r.Outcome.Summary == "" {
		return r.Outcome.Kind
	}
	return r.Outcome.Kind + ": " + r.Outcome.Summary
}

// TriggerText renders the trigger type, preferring the resumed segment's.
func TriggerText(r client.Run) string {
	t := r.Trigger
	if r.CurrentTrigger != nil {
		t = r.CurrentTrigger
	}
	if t == nil {
		return r.TriggerID
	}
	return t.Type
}

// Header is the column header shared by every run row.
const Header = "STATE      ATT BINDING      EXECUTOR     WORK KIND            TRIGGER                STARTED        DURATION   OUTCOME"

// Row renders one run; it is identical for every executor.
func Row(r client.Run, now time.Time) string {
	return Fit(r.State, 10) + " " + Fit(strconv.FormatUint(uint64(r.Attempt), 10), 3) + " " + Fit(r.ExecutorBinding, 12) + " " + Fit(r.Executor, 12) + " " + Fit(r.WorkKind, 20) + " " + Fit(TriggerText(r), 22) + " " + Fit(ShortTime(r.StartedAt), 14) + " " + Fit(Duration(r, now), 10) + " " + Outcome(r)
}

func usageText(u *client.Usage) string {
	if u == nil {
		return ""
	}
	var parts []string
	if u.InputTokens != nil {
		parts = append(parts, fmt.Sprintf("in %d", *u.InputTokens))
	}
	if u.OutputTokens != nil {
		parts = append(parts, fmt.Sprintf("out %d", *u.OutputTokens))
	}
	if u.CachedTokens != nil {
		parts = append(parts, fmt.Sprintf("cached %d", *u.CachedTokens))
	}
	if u.Cost != nil {
		parts = append(parts, fmt.Sprintf("cost %.4f %s", *u.Cost, u.Currency))
	}
	return strings.Join(parts, ", ")
}

// Detail renders the full record of one run and the Work's attempts.
func Detail(r client.Run, attempts []client.Run, now time.Time) string {
	var b strings.Builder
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "  %-18s %s\n", k, v)
		}
	}
	b.WriteString("Run " + r.ID + "\n")
	line("work", r.WorkID+" ("+r.WorkKind+", "+r.WorkState+")")
	line("scope", r.ScopeID)
	if t := r.Trigger; t != nil {
		line("trigger", t.Type+" from "+t.Source+" ["+t.ID+"]")
	}
	if t := r.CurrentTrigger; t != nil {
		line("resumed by", t.Type+" from "+t.Source+" ["+t.ID+"]")
	}
	line("binding", r.ExecutorBinding)
	line("executor", r.Executor)
	line("config generation", r.ConfigGeneration)
	line("attempt", strconv.FormatUint(uint64(r.Attempt), 10)+" (lease epoch "+r.Lease.Epoch+")")
	line("state", r.State+" / work "+r.WorkState)
	line("outcome", Outcome(r))
	line("started", ShortTime(r.StartedAt))
	line("finished", ShortTime(r.FinishedAt))
	line("duration", Duration(r, now))
	line("deadline", ShortTime(r.Deadline))
	if r.RemainingTimeoutSeconds > 0 {
		line("timeout left", (time.Duration(r.RemainingTimeoutSeconds) * time.Second).String())
	}
	line("usage", usageText(r.Usage))
	if ws := r.Workspace; ws != nil {
		v := strings.TrimSpace(ws.Kind + " " + ws.Dir)
		if ws.Summary != "" {
			v += " (" + ws.Summary + ")"
		}
		line("workspace", v)
	}
	// The sandbox each segment ran under, once per change of plan.
	var last string
	for _, g := range r.Segments {
		if g.Sandbox != nil && g.Sandbox.Digest != last {
			last = g.Sandbox.Digest
			line("sandbox", g.Sandbox.Summary+" ("+short(last)+")")
		}
		if c := g.Container; c != nil && c.Digest+c.Daemon != last {
			last = c.Digest + c.Daemon
			line("container", fmt.Sprintf("%s (%s), image %s, daemon %s", c.Summary, short(c.Digest), c.Image, c.Daemon))
		}
	}
	line("inputs", strconv.Itoa(r.Inputs))
	line("wait", r.WaitID)
	if rt := r.Retry; rt != nil {
		v := fmt.Sprintf("failures %d", rt.FailureCount)
		if rt.EligibleAt != "" {
			v += ", eligible " + ShortTime(rt.EligibleAt)
		}
		if rt.Blocked {
			v += ", blocked (" + rt.BlockedReason + ")"
		}
		if rt.Exhausted {
			v += ", exhausted"
		}
		line("retry", v)
	}
	if !r.ResourceExcluded && (r.State == "failed" || r.State == "cancelled") {
		line("resource", "stop not yet confirmed")
	}
	line("retention", r.Retention.State+map[bool]string{true: " " + ShortTime(r.Retention.ExpiredAt), false: ""}[r.Retention.ExpiredAt != ""])
	line("trace", r.TraceID)
	if len(r.Artifacts) > 0 {
		b.WriteString("\nArtifacts\n")
		for _, a := range r.Artifacts {
			fmt.Fprintf(&b, "  %s  %s  %d bytes\n", a.ID, a.MediaType, a.Size)
		}
	}
	if len(attempts) > 0 {
		b.WriteString("\nAttempts for this work\n  " + Header + "\n")
		for _, a := range attempts {
			marker := "  "
			if a.ID == r.ID {
				marker = "> "
			}
			b.WriteString(marker + Row(a, now) + "\n")
		}
	}
	return b.String()
}

// WorkLabel is the human name of Work: the first annotation an extension
// gives it (not the runtime's own, such as a capacity wait), with an
// "owner/repo#12" reference shortened to "repo#12", else its first
// correlation key, else its ID.
func WorkLabel(w client.Work) string {
	for _, a := range w.Annotations {
		if a.Value == "" || a.Source == "runtime" {
			continue
		}
		if repo, ref, ok := strings.Cut(a.Value, "#"); ok && !strings.ContainsAny(a.Value, " :") {
			return repo[strings.LastIndex(repo, "/")+1:] + "#" + ref
		}
		return a.Value
	}
	for _, k := range w.CorrelationKeys {
		return k.Source + " " + k.ExternalID
	}
	return w.ID
}

// short is a sandbox digest cut to its first twelve hex digits.
func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "sha256:" + d
}
