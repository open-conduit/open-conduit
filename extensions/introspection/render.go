package introspection

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alsiola/open-conduit/domain"
)

// RenderEvent renders the extension's Events for an agent processing them:
// a report (its `introspection.report` Work's input), a sweep or triage slot
// (the standing run's trigger) and a requested fix (its fix Work's input).
func RenderEvent(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeReported:
		var r Report
		if e.Data.Schema != SchemaReport || json.Unmarshal(e.Data.Data, &r) != nil {
			return "", false
		}
		by := fmt.Sprintf("Reported by execution %s of Work %s (trigger %s)", r.Execution, r.Work, r.Trigger)
		if r.Origin != "" {
			by = "Reported by the extension's " + r.Origin
		}
		return fmt.Sprintf("### Issue report %s (%s): %s\n\n%s\n\n%s; report Event %s. When you have handled it, record how with `%s` (id %s).",
			r.ID, r.Kind, r.Title, r.Body, by, e.ID, OpResolve, r.ID), true
	case TypeSweepDue, TypeTriageDue:
		d, ok := DuePayload(e)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("### Daily %s, slot %s\n\nStart with `%s` (for %q): it returns this run's evidence and settings (notes namespaces, rule files, guide, how to land a commit, whether to file issues).",
			d.Run, d.Slot, OpEvidence, d.Run), true
	case TypeFixRequested:
		var f Fix
		if e.Data.Schema != SchemaFix || json.Unmarshal(e.Data.Data, &f) != nil {
			return "", false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "### Fix %s: %s\n\n%s\n\nRequested by execution %s of Work %s; fix Event %s", f.ID, f.Title, f.Body, f.Execution, f.Work, e.ID)
		if e.CausationID != "" {
			fmt.Fprintf(&b, " (caused by report Event %s)", e.CausationID)
		}
		b.WriteString(".")
		if len(f.Reports) > 0 {
			fmt.Fprintf(&b, " It fixes reports %s: when the fix has landed, resolve each with `%s` (id, a summary and the commit or PR as link).", strings.Join(f.Reports, ", "), OpResolve)
		}
		if len(f.Notes) > 0 {
			fmt.Fprintf(&b, " It also addresses notes %s.", strings.Join(f.Notes, ", "))
		}
		return b.String(), true
	}
	return "", false
}
