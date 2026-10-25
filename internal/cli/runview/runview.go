// Package runview renders unified run history records as plain text. The
// same row, detail and log selection serve every executor; nothing here
// branches on executor, work kind or domain payloads.
package runview

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/client"
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

func shortTime(s string) string {
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
	return Fit(r.State, 10) + " " + Fit(strconv.FormatUint(uint64(r.Attempt), 10), 3) + " " + Fit(r.ExecutorBinding, 12) + " " + Fit(r.Executor, 12) + " " + Fit(r.WorkKind, 20) + " " + Fit(TriggerText(r), 22) + " " + Fit(shortTime(r.StartedAt), 14) + " " + Fit(Duration(r, now), 10) + " " + Outcome(r)
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
	line("started", shortTime(r.StartedAt))
	line("finished", shortTime(r.FinishedAt))
	line("duration", Duration(r, now))
	line("deadline", shortTime(r.Deadline))
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
	line("inputs", strconv.Itoa(r.Inputs))
	line("wait", r.WaitID)
	if rt := r.Retry; rt != nil {
		v := fmt.Sprintf("failures %d", rt.FailureCount)
		if rt.EligibleAt != "" {
			v += ", eligible " + shortTime(rt.EligibleAt)
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
	line("retention", r.Retention.State+map[bool]string{true: " " + shortTime(r.Retention.ExpiredAt), false: ""}[r.Retention.ExpiredAt != ""])
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
