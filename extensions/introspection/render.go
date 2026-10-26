package introspection

import (
	"encoding/json"
	"fmt"

	"github.com/alsiola/open-conduit/domain"
)

// RenderEvent renders a report Event for an agent processing it (its
// `introspection.report` Work's input).
func RenderEvent(e domain.Event) (string, bool) {
	if e.Type != TypeReported || e.Data.Schema != SchemaReport {
		return "", false
	}
	var r Report
	if json.Unmarshal(e.Data.Data, &r) != nil {
		return "", false
	}
	return fmt.Sprintf("### Issue report %s (%s): %s\n\n%s\n\nReported by execution %s of Work %s (trigger %s); report Event %s. When you have handled it, record how with `%s` (id %s).",
		r.ID, r.Kind, r.Title, r.Body, r.Execution, r.Work, r.Trigger, e.ID, OpResolve, r.ID), true
}
