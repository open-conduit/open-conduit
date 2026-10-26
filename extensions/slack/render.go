package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// RenderEvent renders one Slack Event the way an agent reads it at the start
// of a turn, with the tool that answers it. ok is false for Events this
// extension does not own; the host falls back to the generic renderer.
func RenderEvent(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeMessageHuman, TypeMessageAgent, TypeMessageBot, TypeMessageDeleted:
		var p MessagePayload
		if e.Data.Schema != SchemaMessage || json.Unmarshal(e.Data.Data, &p) != nil {
			return "", false
		}
		if p.Deleted {
			return "### A message in the thread was deleted", true
		}
		where := "Reply in the thread"
		if p.Root {
			where = "Message starting the thread"
		}
		who := author(p)
		if p.Agent != nil {
			who += " (an agent)"
		}
		at := ""
		if t, ok := TSTime(p.TS); ok {
			at = t.Format(time.RFC3339)
		}
		edited := ""
		if p.Edited != "" {
			edited = ", edited"
		}
		text := strings.TrimSpace(p.Text)
		if len(p.Files) > 0 {
			names := make([]string, 0, len(p.Files))
			for _, f := range p.Files {
				names = append(names, f.Name)
			}
			text += "\n\n(Files: " + strings.Join(names, ", ") + ")"
		}
		return fmt.Sprintf("### %s by %s (%s%s; reply with `call %s {\"text\": …}`)\n%s", where, who, at, edited, OpReply, text), true
	case TypeMessageRemoved:
		return "### A message in the thread was deleted", true
	}
	return "", false
}
